// Package contracts wires the contract checker into `go test ./...`.
package contracts

import (
	"os"
	"os/exec"
	"testing"
)

// TestContractInSync runs the python checker; skipped when python3 is not
// installed.
func TestContractInSync(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	if _, err := os.Stat("../scripts/contract-check.py"); err != nil {
		t.Skip("checker script not found")
	}
	cmd := exec.Command("python3", "../scripts/contract-check.py")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("contract drift:\n%s", out)
	}
	t.Log(string(out))
}
