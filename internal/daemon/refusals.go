package daemon

import (
	"errors"
	"net/http"
	"regexp/syntax"
	"strings"

	"sidegate/internal/domain"
)

// Reason names why the daemon refused, one name per cause, beside the message the CLI and the log
// read. The page words each in web/src/lib/troubles.ts's REFUSAL_WORDS, and web/tests/wire/enums.json
// holds the two lists equal. Every error body carries one.
type Reason string

const (
	// The request itself.
	RefusedNoRoute      Reason = "noRoute"
	RefusedBadQuery     Reason = "badQuery"
	RefusedKeyMissing   Reason = "keyMissing"
	RefusedKeyReused    Reason = "keyReused"
	RefusedToken        Reason = "tokenExpired"
	RefusedTooLarge     Reason = "tooLarge"
	RefusedNotJSON      Reason = "notJSON"
	RefusedMalformed    Reason = "malformed"
	RefusedNoRevision   Reason = "preconditionMissing"
	RefusedStale        Reason = "stale"
	RefusedUnavailable  Reason = "unavailable"
	RefusedBusy         Reason = "busy"
	RefusedNotSaved     Reason = "notSaved"
	RefusedUnfinished   Reason = "probeUnfinished"
	RefusedBadID        Reason = "badId"
	RefusedBadUserAgent Reason = "badUserAgent"
	RefusedBadRefresh   Reason = "badRefresh"
	RefusedNotHTTPS     Reason = "notHttps"

	// What the write names.
	RefusedSlotGone    Reason = "slotGone"
	RefusedGroupGone   Reason = "groupGone"
	RefusedSourceGone  Reason = "sourceGone"
	RefusedServerGone  Reason = "serverGone"
	RefusedUntestable  Reason = "notTestable"
	RefusedGroupEmpty  Reason = "groupEmpty"
	RefusedEmpty       Reason = "sourceEmpty"
	RefusedInUse       Reason = "inUse"
	RefusedRouted      Reason = "routed"
	RefusedSourcesGone Reason = "sourcesGone"

	// A press of a server in a group or source, refused after its check passed.
	RefusedOwnerIdle     Reason = "ownerIdle"     // No slot follows the group or source.
	RefusedNotMember     Reason = "notMember"     // The server isn't one the group or source would use.
	RefusedServerChanged Reason = "serverChanged" // The server got a new revision, or the core restarted, during the check.
	RefusedFailedSince   Reason = "failedSince"   // The server failed after the check began.

	// Room.
	RefusedClientsFull Reason = "clientsFull"
	RefusedSlotsFull   Reason = "slotsFull"

	// A VPN Client in the wrong state for its fix.
	RefusedNotDisabled  Reason = "clientNotDisabled"
	RefusedClientExists Reason = "clientPresent"

	// An update.
	RefusedNoReleases   Reason = "noReleases"   // A development build, which takes none.
	RefusedNothingNewer Reason = "nothingNewer" // The last check offers no release.
	RefusedOfferChanged Reason = "offerChanged" // What was confirmed isn't what is offered now.
	RefusedLifecycle    Reason = "lifecycleRunning"

	// A field.
	RefusedNoName         Reason = "nameMissing"
	RefusedNoURL          Reason = "urlMissing"
	RefusedNoText         Reason = "textMissing"
	RefusedNoRule         Reason = "ruleMissing" // A new group whose rule can hold no server.
	RefusedNotRemote      Reason = "notRemote"
	RefusedOriginFixed    Reason = "originFixed"
	RefusedRetentionRange Reason = "retentionRange"
	RefusedKeyRefused     Reason = "keyRefused"   // UniFi refused the API key the owner sent.
	RefusedNotAnswering   Reason = "notAnswering" // UniFi didn't answer a try of the key the owner sent.
	RefusedNoServers      Reason = "noUsableServers"
	RefusedUnreadable     Reason = "unreadableText"
	// A name pattern Go can't compile. Its detail is the parse error cut to its cause.
	RefusedPattern Reason = "badPattern"
)

// CommandKind names a refusal by the kinds web/API.md defines, so the handler can map
// it to its status without reading a message.
type CommandKind string

const (
	KindValidation  CommandKind = "validation"
	KindStale       CommandKind = "staleRevision"
	KindUnsupported CommandKind = "unsupported"
	KindCapacity    CommandKind = "capacity"
	KindReferenced  CommandKind = "referenced"
	KindUnavailable CommandKind = "unavailable"
	KindNotFound    CommandKind = "notFound"
)

// CommandError is the daemon's own refusal. Message is for the CLI and the log, and the page words
// Reason. Revision is what is stored now, on a stale write.
type CommandError struct {
	Kind     CommandKind
	Reason   Reason
	Message  string
	Revision uint64
}

func refuse(kind CommandKind, reason Reason, message string) error {
	return &CommandError{Kind: kind, Reason: reason, Message: message}
}

// errorBody is every error's JSON, with revision only on a stale write, detail only on a pattern
// Go can't read, and diagnostics only on pasted text with no usable servers.
type errorBody struct {
	Kind        string          `json:"kind"`
	Reason      Reason          `json:"reason"`
	Message     string          `json:"message"`
	Detail      string          `json:"detail,omitempty"`
	Revision    *uint64         `json:"revision,omitempty"`
	Diagnostics []DiagnosticDTO `json:"diagnostics,omitempty"`
}

// admission refuses what a domain constructor wouldn't admit from the page: a pattern Go can't read
// names its cause, a slot with no name says so, and anything else is a request the page never makes.
func admission(err error) *Answer {
	var parse *syntax.Error
	switch {
	case errors.As(err, &parse):
		return answerOf(http.StatusUnprocessableEntity, errorBody{Kind: string(KindValidation), Reason: RefusedPattern, Message: sentence(err), Detail: patternCause(parse)})
	case errors.Is(err, domain.ErrNoName):
		return invalid(RefusedNoName, sentence(err))
	}
	return invalid(RefusedMalformed, sentence(err))
}

// patternCause is Go's parse error in the fewest words that say it. A code with no shorter words
// is Go's own.
func patternCause(e *syntax.Error) string {
	switch e.Code {
	case syntax.ErrInvalidPerlOp:
		switch e.Expr {
		case "(?=", "(?!":
			return "lookahead not supported"
		}
		return "unsupported syntax"
	case syntax.ErrInvalidNamedCapture:
		if strings.HasPrefix(e.Expr, "(?<=") || strings.HasPrefix(e.Expr, "(?<!") {
			return "lookbehind not supported"
		}
	case syntax.ErrInvalidRepeatOp:
		return "invalid repeat"
	case syntax.ErrMissingRepeatArgument:
		return "nothing to repeat"
	case syntax.ErrInvalidEscape:
		return "invalid escape"
	case syntax.ErrInvalidCharRange:
		return "invalid range"
	case syntax.ErrTrailingBackslash:
		return "trailing backslash"
	case syntax.ErrNestingDepth:
		return "nested too deeply"
	}
	return string(e.Code)
}
