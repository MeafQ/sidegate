// Package deploy holds what the installer writes or checks that isn't Go.
package deploy

import (
	_ "embed"
	"regexp"
)

// NginxHook is the template for the file nginx includes. See its own comments.
//
//go:embed nginx/shared-runnable-sidegate.conf
var NginxHook string

// xrayPin is shell, because scripts/build-bundle.sh sources it too.
//
//go:embed xray.pin
var xrayPin string

// XraySHA256 is the digest of the only core binary a bundle may carry.
var XraySHA256 = regexp.MustCompile(`(?m)^XRAY_SHA256=(\w+)$`).FindStringSubmatch(xrayPin)[1]
