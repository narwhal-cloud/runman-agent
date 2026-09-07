package updater

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleaseChannels(t *testing.T) {
	for _, version := range []string{"main", "continuous", "continuous-" + strings.Repeat("a", 40), "v1.2.3"} {
		t.Run(version, func(t *testing.T) {
			api, base := releaseAPIURL(version), releaseDownloadBase(version)
			if !strings.Contains(api, "repos/podcctv/runman-agent/") || !strings.Contains(base, "/podcctv/runman-agent/") {
				t.Fatalf("wrong repository: %s %s", api, base)
			}
			if strings.HasPrefix(version, "v") {
				if !strings.HasSuffix(api, "/latest") || !strings.HasSuffix(base, "/latest/download") {
					t.Fatal("stable channel changed")
				}
			} else if !strings.HasSuffix(api, "/tags/continuous") || !strings.HasSuffix(base, "/download/continuous") {
				t.Fatal("rolling channel changed")
			}
			args := strings.Join(updateCommandArgs(version), " ")
			for _, required := range []string{"--unit=narwhal-agent-update", "--update-only", "--non-interactive", "--setenv=RUNMAN_AGENT_DOWNLOAD_BASE=" + base} {
				if !strings.Contains(args, required) {
					t.Fatalf("missing %s: %s", required, args)
				}
			}
			if strings.Contains(args, "narwhal-cloud/") {
				t.Fatal("upstream update source leaked")
			}
		})
	}
}

func TestRollingVersionIdentity(t *testing.T) {
	sha := strings.Repeat("a", 40)
	got, err := releaseVersion(githubRelease{TagName: "continuous", TargetCommitish: sha}, "continuous")
	if err != nil || got != "continuous-"+sha {
		t.Fatalf("%q %v", got, err)
	}
	newVersion, err := releaseVersion(githubRelease{TagName: "continuous", TargetCommitish: strings.Repeat("b", 40)}, "continuous")
	if err != nil || newVersion == got {
		t.Fatal("different commits compare equal")
	}
	for _, rel := range []githubRelease{
		{}, {TagName: "continuous", TargetCommitish: "main"},
		{TagName: "continuous", TargetCommitish: "abc"},
		{TagName: "v1", TargetCommitish: sha},
	} {
		if _, err := releaseVersion(rel, "continuous"); err == nil {
			t.Fatalf("accepted invalid release: %+v", rel)
		}
	}
	if _, err := releaseVersion(githubRelease{}, "latest"); err == nil {
		t.Fatal("accepted empty stable version")
	}
}

func TestDownloadScriptIsAtomic(t *testing.T) {
	for _, scenario := range []string{"ok", "http-error", "empty", "too-large", "truncated"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			dest := filepath.Join(dir, "install.sh")
			old := "old upstream script - must never run on error"
			if err := os.WriteFile(dest, []byte(old), 0600); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch scenario {
				case "ok":
					_, _ = w.Write([]byte("#!/bin/bash\necho updated\n"))
				case "http-error":
					w.WriteHeader(503)
				case "empty":
					w.WriteHeader(200)
				case "too-large":
					_, _ = w.Write([]byte(strings.Repeat("x", (2<<20)+1)))
				case "truncated":
					w.Header().Set("Content-Length", "1000")
					_, _ = w.Write([]byte("partial"))
				}
			}))
			defer server.Close()
			err := downloadScript(context.Background(), server.Client(), server.URL, dest)
			data, readErr := os.ReadFile(dest)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if scenario == "ok" {
				if err != nil || !strings.Contains(string(data), "echo updated") {
					t.Fatalf("%v %s", err, data)
				}
			} else if err == nil || string(data) != old {
				t.Fatalf("failed download replaced original: %v %q", err, data)
			}
			files, _ := filepath.Glob(filepath.Join(dir, ".installer-*"))
			if len(files) != 0 {
				t.Fatalf("temporary files leaked: %v", files)
			}
		})
	}
}
