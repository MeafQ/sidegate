package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"sidegate/internal/domain"
)

// corePass is one pass's access to the core. Every read and write the pass makes goes through it, in
// the order it happens, and every call that answers is heard here, whichever caller made it. coreAPI
// keeps the reads; the writes exist only here.
type corePass struct {
	x     *xray
	view  *view
	gates *gates
}

func (w *Worker) corePass() *corePass { return &corePass{x: w.core, view: &view{}, gates: w.gates} }

// hear is the one place the pass hears the core: the view, then the gates.
func (c *corePass) hear(e event) {
	c.view.hear(e)
	c.gates.hear(e)
}

// unit reads the core's unit and hears the start it names.
func (c *corePass) unit(ctx context.Context) (coreObservation, error) {
	obs, err := c.x.unit(ctx)
	if err == nil {
		c.hear(startRead{epoch: obs.Epoch})
	}
	return obs, err
}

// start reads which start of the core unit runs, "" for none, and hears it.
func (c *corePass) start(ctx context.Context) (domain.CoreEpoch, error) {
	props, err := c.x.unitProps(ctx)
	if err != nil {
		return "", err
	}
	epoch := running(props)
	c.hear(startRead{epoch: epoch})
	return epoch, nil
}

// listInbounds lists the core's inbounds and hears the list.
func (c *corePass) listInbounds(ctx context.Context) ([]string, error) {
	listed, err := c.x.api.listInbounds(ctx)
	if err == nil {
		c.hear(inboundList(listed))
	}
	return listed, err
}

// read is observe with its failure kept in the observation.
func (c *corePass) read(ctx context.Context, want corePlan) coreObservation {
	obs, err := c.observe(ctx, want)
	obs.readErr = err
	return obs
}

// inventory is adoption's read: the core's start and what it holds, of no slot.
func (c *corePass) inventory(ctx context.Context) (coreObservation, error) {
	return c.observe(ctx, corePlan{})
}

// observe reads the unit and, when it is active, the API. want only says which
// balancers to read back; nothing from it enters the observation.
func (c *corePass) observe(ctx context.Context, want corePlan) (coreObservation, error) {
	obs, err := c.unit(ctx)
	if err != nil {
		return obs, err
	}
	if !obs.Running {
		c.hear(goodRead{obs: obs})
		return obs, nil
	}

	if obs.inbounds, err = c.listInbounds(ctx); err != nil {
		return obs, fmt.Errorf("core API: %w", err)
	}
	if obs.outbounds, err = c.x.api.listOutbounds(ctx); err != nil {
		return obs, fmt.Errorf("core API: %w", err)
	}
	c.hear(outboundList(obs.outbounds))
	if obs.rules, err = c.x.api.listRules(ctx); err != nil {
		return obs, fmt.Errorf("core API: %w", err)
	}
	c.hear(ruleList(obs.rules))
	if slices.Contains(obs.inbounds, probeTag) {
		if obs.users, err = c.x.api.inboundUsers(ctx, probeTag); err != nil {
			return obs, fmt.Errorf("core API: %w", err)
		}
	}
	obs.Capabilities = Capabilities{Protocols: renderedProtocols, HotOutbounds: true, HotInbounds: true,
		ProbeUserRouting: slices.Contains(obs.inbounds, probeTag)}

	// A slot is applied when every call about it answered and none rules it out, and the slot rules
	// are the plan's whole, since a pass whose rules differ owes a rewrite that resets every override.
	routing := slices.Equal(slotRules(obs.rules), slotRules(want.rules()))
	for _, s := range want.Slots {
		if !obs.listening(s.ID) {
			continue
		}
		routes, live, err := c.routed(ctx, s.ID, obs.outbounds)
		if err != nil {
			return obs, fmt.Errorf("core API: %w", err)
		}
		obs.Serving[s.ID] = live
		info, err := c.balancer(ctx, s)
		if isNotFound(err) {
			continue
		}
		if err != nil {
			return obs, fmt.Errorf("core API: %w", err)
		}
		obs.Capabilities.OverrideReadback = true
		answers := append([]answer{inboundList(obs.inbounds), outboundList(obs.outbounds), ruleList(obs.rules),
			balancerAnswer{slot: s.ID, override: info.Override, found: true}}, routes...)
		if routing && len(routes) == len(slotNetworks)*len(routeSamples) &&
			!slices.ContainsFunc(answers, func(a answer) bool { return a.rulesOut(s.ID, s.Selection) }) {
			obs.Applied[s.ID] = s.Selection
		}
	}
	for _, ref := range want.Probes {
		e := probeEmail(ref)
		if slices.Contains(obs.users, e) && slices.Contains(obs.rules, routeRule{RuleTag: e, Outbound: outboundTag(ref)}) &&
			slices.Contains(obs.outbounds, outboundTag(ref)) {
			obs.Probes = append(obs.Probes, ref)
		}
	}
	c.hear(goodRead{obs: obs})
	return obs, nil
}

// balancer reads s's balancer and hears it, NotFound included.
func (c *corePass) balancer(ctx context.Context, s coreSlot) (balancerInfo, error) {
	info, err := c.x.api.balancer(ctx, balancerTag(s))
	switch {
	case err == nil:
		c.hear(balancerAnswer{slot: s.ID, override: info.Override, found: true})
	case isNotFound(err):
		c.hear(balancerAnswer{slot: s.ID})
	}
	return info, err
}

// routeSamples are the destination ports routed asks the router about: HTTPS
// and one from the dynamic range.
var routeSamples = []uint32{443, 49152}

// routed asks the router where slot's next TCP and UDP connections to each
// sample port go, and returns every answer it got. live is nothing unless every
// answer is a live outbound: an override naming a missing tag, or no matching
// rule, sends traffic to the default outbound, block. It is a check, not a proof.
// ListRule shows no matchers, and a rule can match only the destinations nobody
// asked about, so readback never proves the core routes as planned. That is why
// reconcile rewrites routing from the plan before a gate opens.
func (c *corePass) routed(ctx context.Context, slot domain.SlotID, outbounds []string) (routes []answer, live []string, err error) {
	for _, network := range slotNetworks {
		for _, port := range routeSamples {
			tag, ok, err := c.x.api.routeOf(ctx, inboundTag(slot, network.name), network.code, port)
			if err != nil {
				return routes, nil, err
			}
			route := routeAnswer{slot: slot, tag: tag, matched: ok}
			c.hear(route)
			routes = append(routes, route)
			if !ok || !slices.Contains(outbounds, tag) {
				return routes, nil, nil
			}
			if !slices.Contains(live, tag) {
				live = append(live, tag)
			}
		}
	}
	return routes, live, nil
}

// reconcile makes the running core match want and reports what it verified. It
// is the only entry point that changes the core, and it is level-triggered:
// called again with the same plan it changes nothing unless rewrite. Routing is
// rewritten from the plan when rewrite says so, even where readback agrees with
// the plan, and when a core with no serving slot reads back other rules.
// Hot order follows the design: add outbounds, inbounds, and users; replace
// routing; override and read back; then retire what nothing references. One step
// failing stops only what depends on it, so an override lands whether or not the
// routing rewrite did. It always returns the core as last read, its read failure
// inside, so its caller judges the gates from what did land.
func (c *corePass) reconcile(ctx context.Context, want corePlan, obs coreObservation, rewrite bool) (coreObservation, []string, error) {
	var steps []string
	if want.empty() {
		if obs.state.stopped() {
			return obs, nil, nil
		}
		if err := c.systemctl(ctx, "stop"); err != nil {
			return obs, nil, err
		}
		return c.read(ctx, want), []string{"core stop"}, nil
	}
	config, err := render(want)
	if err != nil {
		return obs, nil, err
	}
	if !obs.Running {
		if obs.state.changing() {
			return obs, nil, fmt.Errorf("core unit is %s", obs.state)
		}
		if err := writeAtomic(c.x.configPath, config); err != nil {
			return obs, nil, err
		}
		if err := c.systemctl(ctx, "start"); err != nil {
			return obs, nil, err
		}
		steps = append(steps, coreStartStep)
		if obs, err = c.awaitAPI(ctx, want); err != nil {
			return obs, steps, err
		}
		rewrite = false // The core just read its routing from the plan.
	}

	did, applyErr := c.hotApply(ctx, want, obs, rewrite)
	steps = append(steps, did...)
	if len(did) > 0 || applyErr != nil {
		obs = c.read(ctx, want)
	}
	if applyErr != nil || obs.readErr != nil {
		return obs, steps, applyErr
	}
	if current, _ := os.ReadFile(c.x.configPath); !bytes.Equal(current, config) {
		if err := writeAtomic(c.x.configPath, config); err != nil {
			return obs, steps, err
		}
	}
	return obs, steps, nil
}

func (c *corePass) awaitAPI(ctx context.Context, want corePlan) (coreObservation, error) {
	deadline := time.Now().Add(apiTimeout)
	for {
		obs, err := c.observe(ctx, want)
		if err == nil && obs.Running {
			return obs, nil
		}
		if time.Now().After(deadline) {
			return obs, fmt.Errorf("core API did not come up: %v", err)
		}
		select {
		case <-ctx.Done():
			return obs, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (c *corePass) hotApply(ctx context.Context, want corePlan, obs coreObservation, rewrite bool) ([]string, error) {
	var steps []string
	var errs []error
	installed := want.installed()

	var missing []domain.ServerRef
	for _, ref := range installed {
		if !slices.Contains(obs.outbounds, outboundTag(ref)) {
			missing = append(missing, ref)
		}
	}
	if len(missing) > 0 {
		outbounds, err := renderOutbounds(want, missing)
		if err == nil {
			err = c.addConfig(ctx, noSlot, "ado", obj{"outbounds": outbounds})
		}
		if err != nil {
			errs = append(errs, err)
		} else {
			for _, ref := range missing {
				steps = append(steps, "core add outbound "+outboundTag(ref))
			}
		}
	}

	var inbounds []obj
	for _, in := range renderInbounds(want) {
		if !slices.Contains(obs.inbounds, in["tag"].(string)) {
			inbounds = append(inbounds, in)
		}
	}
	did, err := c.addInbounds(ctx, inbounds)
	steps = append(steps, did...)
	if err != nil {
		errs = append(errs, err)
	}
	if slices.Contains(did, "core add inbound "+probeTag) {
		users, err := c.x.api.inboundUsers(ctx, probeTag)
		if err != nil {
			errs = append(errs, err)
		} else {
			obs.users = users
		}
	}
	for _, ref := range want.probeUsers() {
		if e := probeEmail(ref); !slices.Contains(obs.users, e) {
			if err := c.addUser(ctx, e, uuidString(probeUser(ref))); err != nil {
				errs = append(errs, err)
				continue
			}
			steps = append(steps, "core add probe user "+e)
		}
	}

	rewritten := true
	if rewrite || len(want.Slots) == 0 && !slices.Equal(slotRules(obs.rules), slotRules(want.rules())) {
		if err := c.addConfig(ctx, everySlot, "adrules", obj{"routing": renderRouting(want)}); err != nil {
			errs, rewritten = append(errs, err), false
		} else {
			steps = append(steps, routingStep)
		}
	} else {
		did, err := c.probeRules(ctx, want, obs)
		steps = append(steps, did...)
		if err != nil {
			errs, rewritten = append(errs, err), false
		}
	}

	for _, s := range want.Slots {
		if _, ok := obs.Applied[s.ID]; ok && len(steps) == 0 {
			continue
		}
		target := servedTag(s.Selection)
		info, err := c.balancer(ctx, s)
		if err == nil && info.Override != target {
			if err = c.override(ctx, s, target); err == nil {
				steps = append(steps, "core override "+balancerTag(s)+" -> "+target)
			}
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	if !rewritten {
		// Routing the core still holds can reference what this plan left behind.
		return steps, errors.Join(errs...)
	}

	keepUsers := map[string]bool{}
	for _, ref := range want.probeUsers() {
		keepUsers[probeEmail(ref)] = true
	}
	for _, e := range obs.users {
		if !keepUsers[e] {
			if err := c.removeUser(ctx, e); err != nil {
				errs = append(errs, err)
				continue
			}
			steps = append(steps, "core remove probe user "+e)
		}
	}
	keepIn := map[string]bool{probeTag: true}
	for _, s := range want.Slots {
		for _, t := range inboundTags(s.ID) {
			keepIn[t] = true
		}
	}
	for _, t := range obs.inbounds {
		if strings.HasPrefix(t, "s.") && !keepIn[t] {
			if err := c.removeInbound(ctx, t); err != nil {
				errs = append(errs, err)
				continue
			}
			steps = append(steps, "core remove inbound "+t)
		}
	}
	keepOut := map[string]bool{}
	outbounds, err := renderOutbounds(want, installed)
	if err != nil {
		return steps, errors.Join(append(errs, err)...)
	}
	for _, o := range outbounds {
		keepOut[o["tag"].(string)] = true
	}
	for ref, server := range want.Waiting {
		root, hops := server.Closure()
		for _, h := range hops {
			keepOut[hopTag(ref, root, h.ID)] = true
		}
	}
	for _, t := range obs.outbounds {
		if (strings.HasPrefix(t, "o.") || strings.HasPrefix(t, "h.")) && !keepOut[t] {
			if err := c.removeOutbound(ctx, t); err != nil {
				errs = append(errs, err)
				continue
			}
			steps = append(steps, "core remove outbound "+t)
		}
	}
	return steps, errors.Join(errs...)
}

// probeRules adds the probe rules want has and the core lacks, and removes the ones it holds that want
// doesn't, each by its tag, so nothing else in the routing moves. A plan's changes go in one call each
// way. A stale rule goes first, since one of the same tag may be what replaces it.
func (c *corePass) probeRules(ctx context.Context, want corePlan, obs coreObservation) ([]string, error) {
	var steps, stale []string
	var missing []obj
	wanted := want.rules()
	for _, r := range obs.rules {
		if isProbeRule(r.RuleTag) && !slices.Contains(wanted, r) {
			stale = append(stale, r.RuleTag)
		}
	}
	for _, r := range renderRouting(want)["rules"].([]obj) {
		tag, _ := r["ruleTag"].(string)
		outbound, _ := r["outboundTag"].(string)
		if isProbeRule(tag) && (slices.Contains(stale, tag) || !slices.Contains(obs.rules, routeRule{RuleTag: tag, Outbound: outbound})) {
			missing = append(missing, r)
		}
	}
	if len(stale) > 0 {
		args := append([]string{"api", "rmrules", "--server=" + apiAddr.String()}, stale...)
		if _, err := c.x.run(ctx, nil, c.x.binary, args...); err != nil {
			return steps, err
		}
		for _, tag := range stale {
			steps = append(steps, "core remove probe rule "+tag)
		}
	}
	if len(missing) > 0 {
		if err := c.addConfig(ctx, noSlot, "adrules", obj{"routing": obj{"rules": missing}}, "-append"); err != nil {
			return steps, err
		}
		for _, r := range missing {
			steps = append(steps, "core add probe rule "+r["ruleTag"].(string))
		}
	}
	return steps, nil
}

// relisten replaces a slot's UDP inbound, the one way to move its UDP. The core holds one session per
// client address, and a destroyed reply socket only gets it another (PacketWriter), but a closed
// worker's sessions get no more datagrams, so the next one starts a session on the current override.
// The TCP inbound stays, so a connection opened meanwhile meets its listener. Datagrams that arrive
// in the UDP gap, a few ms (lab step swapgap), are dropped. before is the start its caller read just
// before, nil when that read failed. The write is heard once, before the removal, so the add follows
// the removal straight away.
func (c *corePass) relisten(ctx context.Context, s coreSlot, before *domain.CoreEpoch) error {
	c.hear(slotScope(s.ID))
	if err := c.send(ctx, noSlot, handlerService+"RemoveInbound", pbString(nil, 1, inboundTag(s.ID, "udp"))); err != nil {
		return err
	}
	_, err := c.addInbound(ctx, slotInbound(s, "udp"), noSlot, before, true)
	return err
}

// added is what one add of an inbound answered.
type added uint8

const (
	addDone    added = iota // The call answered success.
	addPresent              // "existing tag found": the core held the tag, and this add changed nothing.
	addUnbound              // Any other failure: the tag may be listed with nothing bound.
)

// addInbounds adds each of inbounds the core doesn't list just before, each by its own call, since
// `xray api adi` stops at the first that fails, and a port held under one slot would then keep every
// inbound after it out. A slot whose add fails gets no other add this pass. When a read before an add
// fails, the add is made anyway, and its cleanup has nothing from that read.
func (c *corePass) addInbounds(ctx context.Context, inbounds []obj) ([]string, error) {
	var steps []string
	var failed []domain.SlotID
	var errs []error
	for _, in := range inbounds {
		tag := in["tag"].(string)
		id, slot := inboundSlot(tag)
		if slot && slices.Contains(failed, id) {
			continue
		}
		before, startErr := c.start(ctx)
		listed, listErr := c.listInbounds(ctx)
		if listErr == nil && slices.Contains(listed, tag) {
			continue
		}
		scope := noSlot
		if slot {
			scope = slotScope(id)
		}
		got, err := c.addInbound(ctx, in, scope, known(before, startErr), listErr == nil)
		switch got {
		case addDone:
			steps = append(steps, "core add inbound "+tag)
		case addPresent:
			// The core changed since the list read before, so a read judges the slot.
			steps = append(steps, "core inbound "+tag+" already listed")
		case addUnbound:
			if slot {
				failed = append(failed, id)
			}
		}
		errs = append(errs, err)
	}
	return steps, errors.Join(errs...)
}

// known is a start read's answer, nil when it failed.
func known(epoch domain.CoreEpoch, err error) *domain.CoreEpoch {
	if err != nil {
		return nil
	}
	return &epoch
}

// addInbound adds one inbound, with scope the slots its write is heard on. before is the start read
// just before it, nil when unread, and absent says the list just before lacked its tag. Xray lists an
// inbound's tag before it binds and keeps it when the bind fails, so a slot would read as listening
// with nothing on its port. A failed add is heard as it returns. Its cleanup reads the list and then
// the start again, each tried once more alone if it fails, and the start it reads scopes the failure.
// The tag is removed only when the list before lacked it, the list after holds it and the start is
// the same on both sides, since only this Worker writes inbounds: a core started between them holds
// its config's inbounds, bound, and keeps them.
func (c *corePass) addInbound(ctx context.Context, inbound obj, scope wrote, before *domain.CoreEpoch, absent bool) (added, error) {
	err := c.addConfig(ctx, scope, "adi", obj{"inbounds": []obj{inbound}})
	tag := inbound["tag"].(string)
	switch {
	case err == nil:
		return addDone, nil
	case strings.Contains(err.Error(), "existing tag found"):
		return addPresent, nil
	}
	id, slot := inboundSlot(tag)
	if slot {
		c.hear(addFailed{slot: id})
	}
	listed, listErr := twice(func() ([]string, error) { return c.listInbounds(ctx) })
	now, nowErr := twice(func() (domain.CoreEpoch, error) { return c.start(ctx) })
	if slot && nowErr == nil {
		c.hear(addScoped{slot: id, on: now})
	}
	switch {
	case nowErr != nil:
		return addUnbound, errors.Join(err, nowErr)
	case before == nil:
		return addUnbound, errors.Join(err, fmt.Errorf("the core's start before the add of %s went unread, so nothing is removed", tag))
	case now != *before || now == "":
		return addUnbound, errors.Join(err, fmt.Errorf("the core's start changed during the add of %s: %q, then %q", tag, *before, now))
	case listErr != nil:
		return addUnbound, errors.Join(err, fmt.Errorf("core API: %w", listErr))
	case !absent || !slices.Contains(listed, tag):
		return addUnbound, err
	}
	if rmErr := c.removeInbound(ctx, tag); rmErr != nil {
		return addUnbound, errors.Join(err, fmt.Errorf("remove %s, which the failed add left listed: %w", tag, rmErr))
	}
	return addUnbound, err
}

// twice runs read, and once more if it fails.
func twice[T any](read func() (T, error)) (T, error) {
	v, err := read()
	if err != nil {
		v, err = read()
	}
	return v, err
}

// systemctl starts or stops the core unit, which changes every slot.
func (c *corePass) systemctl(ctx context.Context, verb string) error {
	c.hear(everySlot)
	_, err := c.x.run(ctx, nil, "systemctl", verb, CoreUnitName)
	return err
}

// addConfig runs the pinned binary's `xray api` for the calls that need Xray's
// JSON-to-protobuf conversion. These run only when the plan changes.
func (c *corePass) addConfig(ctx context.Context, scope wrote, command string, payload obj, flags ...string) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	c.hearWrite(scope)
	args := slices.Concat([]string{"api", command}, flags, []string{"--server=" + apiAddr.String(), "stdin:"})
	_, err = c.x.run(ctx, body, c.x.binary, args...)
	return err
}

// override points s's balancer at target.
func (c *corePass) override(ctx context.Context, s coreSlot, target string) error {
	return c.send(ctx, slotScope(s.ID), routingService+"OverrideBalancerTarget", pbString(pbString(nil, 1, balancerTag(s)), 2, target))
}

func (c *corePass) removeInbound(ctx context.Context, tag string) error {
	scope := noSlot
	if id, slot := inboundSlot(tag); slot {
		scope = slotScope(id)
	}
	return c.send(ctx, scope, handlerService+"RemoveInbound", pbString(nil, 1, tag))
}

func (c *corePass) removeOutbound(ctx context.Context, tag string) error {
	return c.send(ctx, noSlot, handlerService+"RemoveOutbound", pbString(nil, 1, tag))
}

// addUser adds a probe user with no flow to the probe inbound through AlterInbound.
func (c *corePass) addUser(ctx context.Context, email, id string) error {
	account := typedMessage("xray.proxy.vless.Account", pbString(nil, 1, id))
	user := pbBytes(pbString(nil, 2, email), 3, account)
	operation := typedMessage("xray.app.proxyman.command.AddUserOperation", pbBytes(nil, 1, user))
	return c.send(ctx, noSlot, handlerService+"AlterInbound", pbBytes(pbString(nil, 1, probeTag), 2, operation))
}

func (c *corePass) removeUser(ctx context.Context, email string) error {
	operation := typedMessage("xray.app.proxyman.command.RemoveUserOperation", pbString(nil, 1, email))
	return c.send(ctx, noSlot, handlerService+"AlterInbound", pbBytes(pbString(nil, 1, probeTag), 2, operation))
}

// send makes one gRPC write, heard with its scope before it is sent.
func (c *corePass) send(ctx context.Context, scope wrote, method string, request []byte) error {
	c.hearWrite(scope)
	_, err := c.x.api.call(ctx, method, request)
	return err
}

// hearWrite hears a write that can change a slot. A write that changes none is not an event.
func (c *corePass) hearWrite(scope wrote) {
	if scope.every || len(scope.slots) > 0 {
		c.hear(scope)
	}
}
