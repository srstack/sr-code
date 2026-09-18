package main

import (
	"strings"
	"testing"
)

func TestEmbedSpecsAllEnabled(t *testing.T) {
	specs, mounts := embedSpecs("dsh", 7781, "/home/user/project", "opencode", 7782, "/home/user/oc-project", "", "kimi", 7783)
	if len(specs) != 3 || len(mounts) != 3 {
		t.Fatalf("got %d specs, %d mounts; want 3 each", len(specs), len(mounts))
	}

	dsh := specs[0]
	if dsh.Name != "dsh" || dsh.Title != "DeepSeek Harness" || dsh.Cmd != "dsh" {
		t.Errorf("dsh spec identity wrong: %+v", dsh)
	}
	if dsh.Dir != "/home/user/project" {
		t.Errorf("dsh Dir = %q, want /home/user/project", dsh.Dir)
	}
	if dsh.HealthPath != "/" {
		t.Errorf("dsh HealthPath = %q, want /", dsh.HealthPath)
	}
	if dsh.URLPattern == "" {
		t.Error("dsh URLPattern empty; want a capture pattern for the start URL")
	}
	if !hasArg(dsh.Args, "{port}") {
		t.Errorf("dsh Args %v missing the {port} placeholder (--port is passed explicitly)", dsh.Args)
	}
	if !hasArg(dsh.Args, "127.0.0.1") {
		t.Errorf("dsh Args %v must pin --host 127.0.0.1", dsh.Args)
	}

	oc := specs[1]
	if oc.Name != "opencode" || oc.Title != "OpenCode" || oc.Cmd != "opencode" {
		t.Errorf("opencode spec identity wrong: %+v", oc)
	}
	if oc.Dir != "/home/user/oc-project" {
		t.Errorf("opencode Dir = %q, want /home/user/oc-project", oc.Dir)
	}
	if !hasArg(oc.Args, "{port}") {
		t.Errorf("opencode Args %v missing the {port} placeholder", oc.Args)
	}

	kimi := specs[2]
	if kimi.Name != "kimi" || kimi.Title != "Kimi Code" || kimi.Cmd != "kimi" {
		t.Errorf("kimi spec identity wrong: %+v", kimi)
	}
	if !hasArg(kimi.Args, "{port}") {
		t.Errorf("kimi Args %v missing the {port} placeholder", kimi.Args)
	}

	for i, want := range []int{7781, 7782, 7783} {
		if mounts[i].ListenPort != want {
			t.Errorf("mounts[%d].ListenPort = %d, want %d", i, mounts[i].ListenPort, want)
		}
	}
}

func TestEmbedSpecsDisabled(t *testing.T) {
	cases := []struct {
		name string
		args [6]interface{}
		want int
	}{
		{"empty dsh cmd", [6]interface{}{"", 7781, "opencode", 7782, "kimi", 7783}, 2},
		{"zero dsh port", [6]interface{}{"dsh", 0, "opencode", 7782, "kimi", 7783}, 2},
		{"zero opencode port", [6]interface{}{"dsh", 7781, "opencode", 0, "kimi", 7783}, 2},
		{"empty kimi cmd", [6]interface{}{"dsh", 7781, "opencode", 7782, "", 7783}, 2},
		{"zero kimi port", [6]interface{}{"dsh", 7781, "opencode", 7782, "kimi", 0}, 2},
		{"all disabled", [6]interface{}{"", 0, "opencode", 0, "", 0}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			specs, mounts := embedSpecs(
				tc.args[0].(string), tc.args[1].(int), "",
				tc.args[2].(string), tc.args[3].(int), "", "",
				tc.args[4].(string), tc.args[5].(int),
			)
			if len(specs) != tc.want {
				t.Errorf("got %d specs, want %d", len(specs), tc.want)
			}
			if len(mounts) != len(specs) {
				t.Errorf("got %d mounts for %d specs; they must stay aligned", len(mounts), len(specs))
			}
		})
	}
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func TestEmbedSpecsOpenCodeV2(t *testing.T) {
	specs, _ := embedSpecs("", 0, "", "opencode", 7782, "/home/user/proj", "/usr/local/bin/opencode2", "", 0)
	if len(specs) != 1 {
		t.Fatalf("got %d specs, want 1", len(specs))
	}
	s := specs[0]
	if s.Name != "opencode" || s.Cmd != "/usr/local/bin/opencode2" {
		t.Fatalf("spec = %+v, want the opencode2 binary", s)
	}
	if len(s.Args) != 0 && s.Args[0] != "serve" {
		t.Errorf("args = %v, want serve ...", s.Args)
	}
	var env, auth string
	for _, e := range s.Env {
		if strings.HasPrefix(e, "OPENCODE_SERVER_PASSWORD=") {
			env = strings.TrimPrefix(e, "OPENCODE_SERVER_PASSWORD=")
		}
	}
	auth = s.BasicAuth
	if _, pass, ok := strings.Cut(auth, ":"); !ok || pass == "" || pass != env {
		t.Errorf("BasicAuth %q must carry the generated password %q", auth, env)
	}
	if !hasArg(s.Args, "{port}") {
		t.Errorf("args %v missing {port}", s.Args)
	}
}
