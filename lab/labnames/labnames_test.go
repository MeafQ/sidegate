package labnames

import (
	"bufio"
	"os"
	"strings"
	"testing"

	"sidegate/internal/daemon"
	gateway "sidegate/internal/runtime"
)

func TestTheScriptsNamesAgree(t *testing.T) {
	file, err := os.Open("../names.sh")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	script := map[string]string{}
	lines := bufio.NewScanner(file)
	for lines.Scan() {
		if name, value, ok := strings.Cut(lines.Text(), "="); ok && !strings.HasPrefix(name, "#") {
			script[name] = value
		}
	}
	for name, want := range map[string]string{
		"LAB_HOME":     DataDir,
		"RUN_DIR":      RunDir,
		"FEEDS":        Feeds,
		"HTTP_SOCKET":  daemon.DefaultListenSocket,
		"BASE_PATH":    daemon.DefaultBasePath,
		"AUTH_HEADER":  daemon.AuthHeader,
		"PROOF_HEADER": daemon.ProofHeader,
		"CSRF_HEADER":  daemon.CSRFHeader,
		"CORE_UNIT":    gateway.CoreUnitName,
	} {
		if script[name] != want {
			t.Errorf("lab/names.sh has %s=%q, and Go has %q", name, script[name], want)
		}
	}
}
