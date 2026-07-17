package version

import (
	"runtime"
	"testing"
)

func TestCurrentIncludesBuildAndToolchainFields(t *testing.T) {
	oldVersion, oldCommit, oldBuildTime := Version, Commit, BuildTime
	t.Cleanup(func() {
		Version, Commit, BuildTime = oldVersion, oldCommit, oldBuildTime
	})

	Version = "1.2.3-test"
	Commit = "abc123"
	BuildTime = "2026-07-18T00:00:00Z"
	got := Current()
	if got.Version != Version || got.Commit != Commit || got.BuildTime != BuildTime {
		t.Fatalf("Current() = %+v", got)
	}
	if got.GoVersion != runtime.Version() {
		t.Fatalf("GoVersion = %q, want %q", got.GoVersion, runtime.Version())
	}
}
