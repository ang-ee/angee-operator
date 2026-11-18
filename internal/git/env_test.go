package git

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A client's Env is set on every command it runs, whichever of its three
// environments the command is built in, and replaces the variable the
// process or a caller gives.
func TestClientEnvIsSetOnEveryCommand(t *testing.T) {
	const print = `printf '%s|%s|%s' "${GIT_SSH_COMMAND-unset}" "${GIT_TERMINAL_PROMPT-unset}" "${OTHER-unset}"`
	keyed := "ssh -F /dev/null -o IdentityFile=/stack/run/ssh/app -o IdentitiesOnly=yes"
	for _, tc := range []struct {
		name           string
		ambient        string // the process's GIT_SSH_COMMAND; "" unsets it
		nonInteractive bool
		env            []string
		want           string // of Run, the inherited environment
		wantIsolated   string // of RunIsolated and runStdout's own reads
	}{
		{name: "a keyed client under a non-interactive platform", nonInteractive: true, env: []string{"GIT_SSH_COMMAND=" + keyed}, want: keyed + "|0|unset", wantIsolated: keyed + "|0|unset"},
		{name: "a keyed client replaces the user's ssh command", ambient: "ssh -F /home/me/.ssh/config", nonInteractive: true, env: []string{"GIT_SSH_COMMAND=" + keyed}, want: keyed + "|0|unset", wantIsolated: keyed + "|0|unset"},
		{name: "a keyed client under an interactive platform", ambient: "ssh -F /home/me/.ssh/config", env: []string{"GIT_SSH_COMMAND=" + keyed, "OTHER=set"}, want: keyed + "|unset|set", wantIsolated: keyed + "|0|set"},
		{name: "a later entry replaces an earlier one", nonInteractive: true, env: []string{"GIT_SSH_COMMAND=first", "GIT_SSH_COMMAND=" + keyed}, want: keyed + "|0|unset", wantIsolated: keyed + "|0|unset"},
		{name: "no env keeps the user's ssh command", ambient: "ssh -F /home/me/.ssh/config", nonInteractive: true, want: "ssh -F /home/me/.ssh/config|0|unset", wantIsolated: "ssh -F /home/me/.ssh/config|0|unset"},
		{name: "no env and no ssh command of the user's", nonInteractive: true, want: "ssh -o BatchMode=yes|0|unset", wantIsolated: "ssh -o BatchMode=yes|0|unset"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unsetEnv(t, "GIT_TERMINAL_PROMPT")
			unsetEnv(t, "OTHER")
			if tc.ambient == "" {
				unsetEnv(t, "GIT_SSH_COMMAND")
			} else {
				t.Setenv("GIT_SSH_COMMAND", tc.ambient)
			}
			client := Client{Bin: "sh", NonInteractive: tc.nonInteractive, Env: tc.env}
			out, err := client.Run(t.Context(), "", "-c", print)
			if err != nil || string(out) != tc.want {
				t.Fatalf("Run() environment = %q, %v; want %q", out, err, tc.want)
			}
			isolated, _, err := client.RunIsolated(t.Context(), "", nil, "-c", print)
			if err != nil || string(isolated) != tc.wantIsolated {
				t.Fatalf("RunIsolated() environment = %q, %v; want %q", isolated, err, tc.wantIsolated)
			}
			// What a caller passes RunIsolated is under the client's own.
			isolated, _, err = client.RunIsolated(t.Context(), "", []string{"GIT_SSH_COMMAND=the caller's"}, "-c", print)
			want := tc.wantIsolated
			if len(tc.env) == 0 {
				want = "the caller's|0|unset"
			}
			if err != nil || string(isolated) != want {
				t.Fatalf("RunIsolated(caller env) environment = %q, %v; want %q", isolated, err, want)
			}
			read, err := client.runStdout(t.Context(), "", stdoutLimit, "-c", print)
			wantRead := tc.want
			if err != nil || string(read) != wantRead {
				t.Fatalf("runStdout() environment = %q, %v; want %q", read, err, wantRead)
			}
		})
	}
}

// For the usual paths the ssh command reads exactly as documented; a path
// that is not plain is quoted for the shell git hands the command to, and
// for ssh, which splits an option's value on spaces, reads its quotes and
// expands % in it.
func TestSSHKeyCommand(t *testing.T) {
	for _, tc := range []struct {
		name, key, knownHosts, want string
	}{
		{
			name: "plain paths", key: "/srv/omelas/run/ssh/angee-arp", knownHosts: "/srv/omelas/run/ssh/known_hosts",
			want: "ssh -F /dev/null -o IdentityFile=/srv/omelas/run/ssh/angee-arp -o IdentitiesOnly=yes -o BatchMode=yes -o 'UserKnownHostsFile=/srv/omelas/run/ssh/known_hosts ~/.ssh/known_hosts' -o StrictHostKeyChecking=accept-new",
		},
		{
			name: "a root with a space", key: "/Users/me/My Stacks/omelas/run/ssh/angee-arp", knownHosts: "/Users/me/My Stacks/omelas/run/ssh/known_hosts",
			want: `ssh -F /dev/null -o 'IdentityFile="/Users/me/My Stacks/omelas/run/ssh/angee-arp"' -o IdentitiesOnly=yes -o BatchMode=yes -o 'UserKnownHostsFile="/Users/me/My Stacks/omelas/run/ssh/known_hosts" ~/.ssh/known_hosts' -o StrictHostKeyChecking=accept-new`,
		},
		{
			name: "an apostrophe, a percent sign, a quote and a backslash", key: `/srv/it's/run/ssh/a%2Fb`, knownHosts: `/srv/it's/a "b"\c/known_hosts`,
			want: `ssh -F /dev/null -o 'IdentityFile="/srv/it'\''s/run/ssh/a%%2Fb"' -o IdentitiesOnly=yes -o BatchMode=yes -o 'UserKnownHostsFile="/srv/it'\''s/a \"b\"\\c/known_hosts" ~/.ssh/known_hosts' -o StrictHostKeyChecking=accept-new`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := SSHKeyCommand(tc.key, tc.knownHosts); got != tc.want {
				t.Fatalf("SSHKeyCommand() =\n%s\nwant\n%s", got, tc.want)
			}
		})
	}
}

// git hands GIT_SSH_COMMAND to the shell, and ssh reads each option's value
// as configuration: whatever the paths hold (a space, an apostrophe, a
// percent sign), ssh gets the options whole and takes the two files they
// name, the key as its only identity.
func TestSSHKeyCommandReachesSSHWhole(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	isolateGitConfig(t)
	base := filepath.Join(t.TempDir(), "a stack's 100% root")
	bin := filepath.Join(base, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(base, "ssh.log")
	// A stand-in for ssh that records its arguments, one per line, and fails.
	script := "#!/bin/sh\nfor arg in \"$@\"; do printf '%s\\n' \"$arg\"; done >> \"$SSH_LOG\"\nexit 255\n"
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	realSSH, _ := exec.LookPath("ssh")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SSH_LOG", log)
	// A source name that is no plain file name is escaped with a percent sign.
	key, knownHosts := filepath.Join(base, "run", "ssh", "team%2Fapp"), filepath.Join(base, "run", "ssh", "known_hosts")
	client := Client{NonInteractive: true, Env: []string{"GIT_SSH_COMMAND=" + SSHKeyCommand(key, knownHosts)}}
	if err := client.Clone(context.Background(), "git@example.invalid:acme/app.git", filepath.Join(base, "clone")); err == nil {
		t.Fatal("Clone() through a failing ssh succeeded")
	}
	recorded, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("the stand-in ssh did not run: %v", err)
	}
	args := strings.Split(strings.TrimSpace(string(recorded)), "\n")
	want := []string{
		"-F", "/dev/null",
		"-o", `IdentityFile="` + strings.ReplaceAll(key, "%", "%%") + `"`,
		"-o", "IdentitiesOnly=yes", "-o", "BatchMode=yes",
		"-o", `UserKnownHostsFile="` + strings.ReplaceAll(knownHosts, "%", "%%") + `" ~/.ssh/known_hosts`,
		"-o", "StrictHostKeyChecking=accept-new",
	}
	if len(args) < len(want) || strings.Join(args[:len(want)], "\n") != strings.Join(want, "\n") {
		t.Fatalf("ssh arguments =\n%s\nwant them to start with\n%s", strings.Join(args, "\n"), strings.Join(want, "\n"))
	}
	if !strings.Contains(string(recorded), "git@example.invalid\n") {
		t.Fatalf("ssh arguments =\n%s\nwant the remote's host", recorded)
	}

	if realSSH == "" {
		t.Skip("ssh not installed: its own reading of the options is not checked")
	}
	// The real ssh, given those options and a server that only says hello:
	// once connected it names the identity files it takes, and it must name
	// the key file, by its real name, and no other: not a default key of the
	// user's, and not one that the ssh configuration of whoever runs this
	// test names for the host, which -F /dev/null keeps it from reading. (ssh
	// finds that configuration through the password database, so the test
	// cannot plant one.)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback listener: %v", err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_, _ = conn.Write([]byte("SSH-2.0-angee-test\r\n"))
			time.Sleep(200 * time.Millisecond)
			_ = conn.Close()
		}
	}()
	if err := os.MkdirAll(filepath.Dir(key), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, []byte("not a key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	sshArgs := append([]string{"-v"}, args[:len(want)]...)
	sshArgs = append(sshArgs, "-o", "ConnectTimeout=5", "-p", port, "git@127.0.0.1", "true")
	out, _ := exec.CommandContext(ctx, realSSH, sshArgs...).CombinedOutput()
	var identities []string
	for _, line := range strings.Split(string(out), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "debug1: identity file "); ok {
			identities = append(identities, rest)
		}
	}
	if len(identities) == 0 {
		t.Skipf("ssh did not get as far as naming its identity files:\n%s", out)
	}
	if len(identities) != 1 || !strings.HasPrefix(identities[0], key+" type ") {
		t.Fatalf("ssh takes the identity files %q, want %s alone:\n%s", identities, key, out)
	}
	if strings.Contains(string(out), "invalid quotes") || strings.Contains(string(out), "Bad configuration option") {
		t.Fatalf("ssh could not read the options:\n%s", out)
	}
	// The stack's known-hosts file is read back by its real name too, first,
	// with the user's own after it.
	config, err := exec.Command(realSSH, append(append([]string{"-G"}, args[:len(want)]...), "example.invalid")...).Output()
	if err != nil {
		t.Fatalf("ssh -G: %v", err)
	}
	var files string
	for _, line := range strings.Split(string(config), "\n") {
		if rest, ok := strings.CutPrefix(line, "userknownhostsfile "); ok {
			files = rest
		}
	}
	if rest, ok := strings.CutPrefix(files, knownHosts+" "); !ok || !strings.HasSuffix(rest, "/.ssh/known_hosts") || strings.Contains(rest, "~") {
		t.Fatalf("ssh -G reports the known-hosts files %q, want %s and then the user's own:\n%s", files, knownHosts, config)
	}
}
