package model

import "testing"

func TestContainerIDStableAndShort(t *testing.T) {
	a := ContainerIDForFile("/tmp/logs/core-api.log")
	b := ContainerIDForFile("/tmp/logs/core-api.log")
	c := ContainerIDForFile("/tmp/logs/core-front.log")
	if a != b {
		t.Fatalf("id not stable: %s != %s", a, b)
	}
	if a == c {
		t.Fatalf("distinct files produced same id: %s", a)
	}
	if len(a) != 12 {
		t.Fatalf("id length = %d, want 12", len(a))
	}
}

func TestContainerName(t *testing.T) {
	cases := map[string]string{
		"/tmp/logs/core-api.log": "core-api",
		"relative/front.txt":     "front",
		"noext":                  "noext",
	}
	for in, want := range cases {
		if got := ContainerNameForFile(in); got != want {
			t.Errorf("name(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestToDozzleLogEventMapping(t *testing.T) {
	m := LogLine{
		Type:    TypeLogSingle,
		Content: "ERROR something failed",
		LogType: Stderr,
		Ts:      1700000000000,
		Origin:  &LogOrigin{File: "/tmp/logs/core-api.log"},
	}
	ev := ToDozzleLogEvent(m, 7)

	if ev.T != "single" {
		t.Errorf("T = %q, want single", ev.T)
	}
	if ev.M != "ERROR something failed" || ev.Rm != m.Content {
		t.Errorf("message not carried: %q / %q", ev.M, ev.Rm)
	}
	if ev.Ts != 1700000000000 {
		t.Errorf("Ts = %d", ev.Ts)
	}
	if ev.Id != 7 {
		t.Errorf("Id = %d, want 7", ev.Id)
	}
	if ev.S != "stderr" {
		t.Errorf("S = %q, want stderr", ev.S)
	}
	if ev.L != "error" {
		t.Errorf("L = %q, want error", ev.L)
	}
	if ev.C != ContainerIDForFile("/tmp/logs/core-api.log") {
		t.Errorf("C = %q, want container id for file", ev.C)
	}
}

func TestLevelHeuristic(t *testing.T) {
	cases := []struct {
		content string
		lt      int
		want    string
	}{
		{"plain line", Stdout, "unknown"},
		{"WARN disk low", Stdout, "warn"},
		{"a fatal crash", Stdout, "fatal"},
		{"nothing special", Stderr, "error"}, // stderr defaults to error
		{"INFO started", Stdout, "info"},
	}
	for _, c := range cases {
		if got := levelFor(c.content, c.lt); got != c.want {
			t.Errorf("levelFor(%q,%d) = %q, want %q", c.content, c.lt, got, c.want)
		}
	}
}
