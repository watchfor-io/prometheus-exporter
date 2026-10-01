package main

import (
	"net"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestRunExitCodes(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()

	cases := []struct {
		name       string
		args       []string
		env        map[string]string
		code       int
		wantStdout string
		wantStderr string
	}{
		{"version", []string{"--version"}, nil, 0, "watchfor-prometheus-exporter dev", ""},
		{"help", []string{"--help"}, nil, 0, "", "Quick start"},
		{"no key", nil, nil, 2, "", "no targets"},
		{"interval too short", []string{"--poll.interval=10s"}, map[string]string{"WATCHFOR_API_KEY": "k"}, 2, "", "below the 1m0s minimum"},
		{"address in use", []string{"--web.listen-address=" + busy.Addr().String()}, map[string]string{"WATCHFOR_API_KEY": "k"}, 1, "", "cannot listen"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut strings.Builder
			code := run(tc.args, env(tc.env), &out, &errOut)
			if code != tc.code {
				t.Errorf("exit code %d, want %d; stderr:\n%s", code, tc.code, errOut.String())
			}
			if !strings.Contains(out.String(), tc.wantStdout) {
				t.Errorf("stdout %q lacks %q", out.String(), tc.wantStdout)
			}
			if !strings.Contains(errOut.String(), tc.wantStderr) {
				t.Errorf("stderr %q lacks %q", errOut.String(), tc.wantStderr)
			}
		})
	}
}
