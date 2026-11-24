//go:build !linux

package runtime

import "errors"

// dumpLinks exists only on the gateway. Elsewhere no link can be sampled, so every link reads unknown.
func dumpLinks() (map[string]linkSample, error) { return nil, errors.New("a link dump needs linux") }
