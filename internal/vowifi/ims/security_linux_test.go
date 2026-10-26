//go:build linux

package ims

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// These tests use a synthetic ip executable, not the host network namespace.
func fakeIPCommand(t *testing.T) (string, string) {
	t.Helper()
	directory := t.TempDir()
	command, log := directory+"/ip", directory+"/commands"
	t.Setenv("VOCAT_IP_TEST_LOG", log)
	t.Setenv("VOCAT_IP_TEST_FAIL", "0")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$VOCAT_IP_TEST_LOG\"\n" +
		"count=$(wc -l < \"$VOCAT_IP_TEST_LOG\")\n" +
		"if [ \"$count\" -eq \"$VOCAT_IP_TEST_FAIL\" ]; then echo \"$*\" >&2; exit 1; fi\n"
	if err := os.WriteFile(command, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return command, log
}

func readIPCommandLog(t *testing.T, path string) []string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(content)), "\n")
}

func TestLinuxIPSecRollbackDeletesOnlyAcknowledgedAdds(t *testing.T) {
	install, err := buildXFRMInstallPlan(testIPSecSAConfig())
	if err != nil {
		t.Fatal(err)
	}
	cleanup := buildXFRMCleanupPlan(testIPSecSAConfig())
	for failAt := 1; failAt <= len(install); failAt++ {
		t.Run(fmt.Sprint(failAt), func(t *testing.T) {
			command, log := fakeIPCommand(t)
			t.Setenv("VOCAT_IP_TEST_FAIL", strconv.Itoa(failAt))
			if _, err := (linuxIPSecInstaller{ipCommand: command}).Install(context.Background(), testIPSecSAConfig()); err == nil {
				t.Fatal("failed add was accepted")
			}
			calls := readIPCommandLog(t, log)
			var want []string
			for _, operation := range install[:failAt] {
				want = append(want, strings.Join(operation.arguments, " "))
			}
			// A failed add may collide with somebody else's state/policy. It
			// and every not-yet-attempted operation MUST NOT be deleted.
			for _, operation := range cleanup[len(install)-(failAt-1):] {
				want = append(want, strings.Join(operation.arguments, " "))
			}
			if !reflect.DeepEqual(calls, want) {
				t.Fatalf("rollback touched unowned entries: calls=%d, want=%d", len(calls), len(want))
			}
		})
	}
}

func TestLinuxIPSecCleanupCanRetryOnlyFailedDeletes(t *testing.T) {
	command, log := fakeIPCommand(t)
	handle, err := (linuxIPSecInstaller{ipCommand: command}).Install(context.Background(), testIPSecSAConfig())
	if err != nil {
		t.Fatal(err)
	}
	adds := len(readIPCommandLog(t, log))
	t.Setenv("VOCAT_IP_TEST_FAIL", strconv.Itoa(adds+2))
	if err := handle.Close(context.Background()); err == nil {
		t.Fatal("failed cleanup was accepted")
	}
	beforeRetry := readIPCommandLog(t, log)
	t.Setenv("VOCAT_IP_TEST_FAIL", "0")
	if err := handle.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	afterRetry := readIPCommandLog(t, log)
	if len(afterRetry) != len(beforeRetry)+1 || afterRetry[len(afterRetry)-1] != beforeRetry[adds+1] {
		t.Fatal("cleanup did not retry exactly the failed deletion")
	}
	if err := handle.Close(context.Background()); err != nil || len(readIPCommandLog(t, log)) != len(afterRetry) {
		t.Fatal("successful close was not idempotent")
	}
}

func TestLinuxIPSecErrorsDoNotEchoKeyArguments(t *testing.T) {
	command, _ := fakeIPCommand(t)
	t.Setenv("VOCAT_IP_TEST_FAIL", "1")
	_, err := (linuxIPSecInstaller{ipCommand: command}).Install(context.Background(), testIPSecSAConfig())
	if err == nil || strings.Contains(err.Error(), "auth-trunc") || strings.Contains(err.Error(), "0x") {
		t.Fatal("iproute2 stderr exposed SA arguments")
	}
}

func TestLinuxIPSecPolicyDoesNotRequireProtocolNameDatabase(t *testing.T) {
	install, err := buildXFRMInstallPlan(testIPSecSAConfig())
	if err != nil {
		t.Fatal(err)
	}
	cleanup := buildXFRMCleanupPlan(testIPSecSAConfig())
	for _, operations := range [][]xfrmOperation{install, cleanup} {
		for _, operation := range operations {
			if !strings.Contains(operation.description, "policy") {
				continue
			}
			joined := " " + strings.Join(operation.arguments, " ") + " "
			if !strings.Contains(joined, " proto 6 ") && !strings.Contains(joined, " proto 17 ") {
				t.Fatal("policy selector requires /etc/protocols", operation.description)
			}
		}
	}
}

func TestRunIPCommandExplainsMissingKernelXFRM(t *testing.T) {
	directory := t.TempDir()
	command := directory + "/ip"
	script := "#!/bin/sh\necho 'Cannot open netlink socket: Protocol not supported' >&2\nexit 1\n"
	if err := os.WriteFile(command, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	err := runIPCommand(context.Background(), command, xfrmOperation{description: "test state"})
	if err == nil || !strings.Contains(err.Error(), "kmod-ipsec") || !strings.Contains(err.Error(), "CONFIG_XFRM_USER") {
		t.Fatalf("error = %v", err)
	}
}

func TestLinuxIPSecInstallerLifecycle(t *testing.T) {
	if os.Getenv("VOCAT_NETNS_TEST") != "1" {
		t.Skip("set VOCAT_NETNS_TEST=1 inside an isolated Linux network namespace")
	}
	handle, err := (linuxIPSecInstaller{ipCommand: "ip"}).Install(
		context.Background(),
		testIPSecSAConfig(),
	)
	if err != nil {
		t.Fatalf("install ipsec-3gpp XFRM set: %v", err)
	}
	states, err := exec.Command("ip", "xfrm", "state").CombinedOutput()
	if err != nil {
		t.Fatalf("list XFRM states: %v: %s", err, states)
	}
	if count := strings.Count(string(states), "src 10.0.0.2 dst 10.0.0.3"); count != 2 {
		t.Fatalf("outbound XFRM state count = %d: %s", count, states)
	}
	if count := strings.Count(string(states), "src 10.0.0.3 dst 10.0.0.2"); count != 2 {
		t.Fatalf("inbound XFRM state count = %d: %s", count, states)
	}
	policies, err := exec.Command("ip", "xfrm", "policy").CombinedOutput()
	if err != nil {
		t.Fatalf("list XFRM policies: %v: %s", err, policies)
	}
	if count := strings.Count(string(policies), "sport 40666 dport 50600"); count != 2 {
		t.Fatalf("UE-client policy count = %d: %s", count, policies)
	}

	closeContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := handle.Close(closeContext); err != nil {
		t.Fatalf("close ipsec-3gpp XFRM set: %v", err)
	}
	states, err = exec.Command("ip", "xfrm", "state").CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(states)) != "" {
		t.Fatalf("XFRM states survived Close: %s", states)
	}
	policies, err = exec.Command("ip", "xfrm", "policy").CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(policies)) != "" {
		t.Fatalf("XFRM policies survived Close: %s", policies)
	}
}

func TestLinuxIPSecRollbackPreservesForeignKernelState(t *testing.T) {
	if os.Getenv("VOCAT_NETNS_TEST") != "1" {
		t.Skip("set VOCAT_NETNS_TEST=1 inside an isolated Linux network namespace")
	}
	config := testIPSecSAConfig()
	install, err := buildXFRMInstallPlan(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := runIPCommand(context.Background(), "ip", install[0]); err != nil {
		t.Fatal(err)
	}
	cleanup := buildXFRMCleanupPlan(config)
	t.Cleanup(func() { _ = runIPCommand(context.Background(), "ip", cleanup[len(cleanup)-1]) })
	before, err := exec.Command("ip", "xfrm", "state").CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (linuxIPSecInstaller{ipCommand: "ip"}).Install(context.Background(), config); err == nil {
		t.Fatal("existing kernel SA did not reject duplicate add")
	}
	after, err := exec.Command("ip", "xfrm", "state").CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("failed install deleted or changed a foreign kernel SA")
	}
}
