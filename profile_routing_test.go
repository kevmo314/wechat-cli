package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kevmo314/wechat-cli/internal/native"
)

func saveProfileFixture(t *testing.T, store Store, profile string) {
	t.Helper()
	s, err := openNativeStore(store)
	if err != nil {
		t.Fatal(err)
	}
	a := &native.Account{
		Profile: profile, Host: "long.weixin.qq.com", Username: "test-owner", UIN: 42,
		Device: bytes.Repeat([]byte{1}, 16), RequestKey: bytes.Repeat([]byte{2}, 24),
		ResponseKey: bytes.Repeat([]byte{3}, 24),
	}
	if err := s.save("state.json", nativeState{Account: a}); err != nil {
		t.Fatal(err)
	}
}

func TestLoginProfileRoutesBeforeCheckingExistingAccount(t *testing.T) {
	for _, tc := range []struct {
		name, profile, slug string
		args                []string
	}{
		{"global", native.DesktopProfile, "desktop", []string{"--profile", "desktop", "login"}},
		{"login-local", native.DesktopProfile, "desktop", []string{"login", "--profile", "desktop"}},
		{"default-login", native.DesktopProfile, "desktop", []string{"login"}},
		{"tablet-alias", native.AndroidTabletProfile, "tablet", []string{"login", "--profile=tablet"}},
		{"phone", native.PhoneProfile, "phone", []string{"login", "--profile", "phone"}},
		{"matching-aliases", native.AndroidTabletProfile, "tablet", []string{"--profile", "tablet", "login", "--profile", "android-tablet"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := Store{filepath.Join(t.TempDir(), "config")}
			otherProfile := native.AndroidTabletProfile
			if tc.profile == otherProfile {
				otherProfile = native.DesktopProfile
			}
			saveProfileFixture(t, base, otherProfile)
			target := Store{filepath.Join(base.Dir, "profiles", tc.slug)}
			saveProfileFixture(t, target, tc.profile)
			// A held reader lock identifies the selected directory before login can
			// contact a server. Both fixture accounts also prevent authentication.
			lock, err := target.readerLock()
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			args := append([]string{"--config-dir", base.Dir}, tc.args...)
			err = run(context.Background(), args)
			if err == nil || !strings.Contains(err.Error(), "another read/login/logout") {
				t.Fatalf("login did not select the requested profile directory: %v", err)
			}
		})
	}
}

func TestLoginProfileArgumentUsesLoginFlagParser(t *testing.T) {
	for _, tc := range []struct {
		name, profile string
		args          []string
		explicit      bool
	}{
		{"default", native.DesktopProfile, nil, false},
		{"alias", native.AndroidTabletProfile, []string{"--profile", "tablet"}, true},
		{"equals", native.PhoneProfile, []string{"--profile=phone"}, true},
		{"last-flag-wins", native.AndroidTabletProfile, []string{"--profile", "desktop", "--profile", "tablet"}, true},
		{"qr-value-is-not-a-flag", native.DesktopProfile, []string{"--qr", "--profile=phone"}, false},
		{"resume", native.DesktopProfile, []string{"--resume"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, explicit, err := loginProfileArgument(tc.args)
			if err != nil || got != tc.profile || explicit != tc.explicit {
				t.Fatalf("profile=%q explicit=%v err=%v", got, explicit, err)
			}
		})
	}
	for _, args := range [][]string{{"--profile"}, {"--profile", "unknown"}, {"--unknown"}, {"--timeout", "bad"}, {"positional"}} {
		if _, _, err := loginProfileArgument(args); err == nil {
			t.Fatalf("accepted invalid login arguments %q", args)
		}
	}
	if _, _, err := loginProfileArgument([]string{"--help"}); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("login help did not return ErrHelp: %v", err)
	}
}

func TestConflictingLoginProfilesFailBeforeOpeningAStore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "unused")
	err := run(context.Background(), []string{"--config-dir", dir, "--profile", "desktop", "login", "--profile", "tablet"})
	if err == nil || !strings.Contains(err.Error(), "global and login --profile options disagree") {
		t.Fatalf("conflicting profiles were not rejected: %v", err)
	}
}

func TestResumeKeepsImplicitProfileUnspecified(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		args       []string
	}{
		{"implicit", "saved QR expired", []string{"login", "--resume"}},
		{"global-explicit", "saved login uses a different profile", []string{"--profile", "desktop", "login", "--resume"}},
		{"local-explicit", "saved login uses a different profile", []string{"login", "--resume", "--profile", "desktop"}},
		{"matching-explicit", "saved QR expired", []string{"--profile", "tablet", "login", "--resume"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := Store{t.TempDir()}
			s, err := openNativeStore(base)
			if err != nil {
				t.Fatal(err)
			}
			// An expired attempt exercises validation without opening a connection.
			if err := s.save("login.json", native.LoginAttempt{Profile: native.AndroidTabletProfile}); err != nil {
				t.Fatal(err)
			}
			args := append([]string{"--config-dir", base.Dir}, tc.args...)
			if err := run(context.Background(), args); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("resume validation: %v; want %q", err, tc.want)
			}
		})
	}
}

func TestPreferredProfileStoreFindsDesktopWithoutPreference(t *testing.T) {
	config := t.TempDir()
	base := Store{filepath.Join(config, "wechat-cli")}
	target := Store{filepath.Join(base.Dir, "profiles", "desktop")}
	saveProfileFixture(t, target, native.DesktopProfile)
	got, err := preferredProfileStore(base, config)
	if err != nil || got.Dir != target.Dir {
		t.Fatalf("desktop session was not discovered: store=%q err=%v", got.Dir, err)
	}
	got, err = preferredProfileStore(base, config, true)
	if err != nil || got.Dir != base.Dir {
		t.Fatalf("explicit empty directory was not honored: store=%q err=%v", got.Dir, err)
	}
}

func TestPreferredProfileStoreReportsDuplicateSessions(t *testing.T) {
	for _, preference := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy-default", true: "remembered-profile"}[preference], func(t *testing.T) {
			config := t.TempDir()
			base := Store{filepath.Join(config, "wechat-cli")}
			saveProfileFixture(t, base, native.DesktopProfile)
			saveProfileFixture(t, Store{filepath.Join(base.Dir, "profiles", "desktop")}, native.DesktopProfile)
			if preference {
				if err := rememberProfile(base, native.DesktopProfile); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := preferredProfileStore(base, config); err == nil || !strings.Contains(err.Error(), "--config-dir") {
				t.Fatalf("preferred profile hid duplicate sessions: %v", err)
			}
			got, err := preferredProfileStore(base, config, true)
			if err != nil || got.Dir != base.Dir {
				t.Fatalf("explicit base was not honored: %v", err)
			}
		})
	}
}

func TestSingleSavedProfileRemainsCompatible(t *testing.T) {
	for _, tc := range []struct {
		name, profile, location string
	}{
		{"legacy-desktop", native.DesktopProfile, "base"},
		{"profile-desktop", native.DesktopProfile, "profile"},
		{"legacy-tablet", native.AndroidTabletProfile, "tablet"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := t.TempDir()
			base := Store{filepath.Join(config, "wechat-cli")}
			expected := base
			switch tc.location {
			case "profile":
				expected = Store{filepath.Join(base.Dir, "profiles", "desktop")}
			case "tablet":
				expected = Store{filepath.Join(config, "wechat-tablet")}
			}
			saveProfileFixture(t, expected, tc.profile)
			got, err := profileStore(base, config, tc.profile, false)
			if err != nil || got.Dir != expected.Dir {
				t.Fatalf("single existing profile was not reused: %v", err)
			}
		})
	}
}

func TestLegacyTabletDuplicateRequiresExplicitDirectory(t *testing.T) {
	config := t.TempDir()
	base := Store{filepath.Join(config, "wechat-cli")}
	legacy := Store{filepath.Join(config, "wechat-tablet")}
	saveProfileFixture(t, base, native.AndroidTabletProfile)
	saveProfileFixture(t, legacy, native.AndroidTabletProfile)
	if _, err := profileStore(base, config, "tablet", false); err == nil || !strings.Contains(err.Error(), legacy.Dir) {
		t.Fatalf("legacy tablet duplicate was hidden: %v", err)
	}
}

func TestDuplicateProfilesRequireExplicitDirectory(t *testing.T) {
	config := t.TempDir()
	base := Store{filepath.Join(config, "wechat-cli")}
	target := Store{filepath.Join(base.Dir, "profiles", "desktop")}
	saveProfileFixture(t, base, native.DesktopProfile)
	saveProfileFixture(t, target, native.DesktopProfile)

	if _, err := profileStore(base, config, "desktop", false); err == nil {
		t.Fatal("silently selected one of two desktop sessions")
	} else if !strings.Contains(err.Error(), "--config-dir") ||
		!strings.Contains(err.Error(), base.Dir) || !strings.Contains(err.Error(), target.Dir) {
		t.Fatalf("missing explicit-directory recovery instructions: %v", err)
	}
	for _, chosen := range []Store{base, target} {
		got, err := profileStore(chosen, config, "desktop", true)
		if err != nil || got.Dir != chosen.Dir {
			t.Fatalf("explicit directory %q was not honored: %v", chosen.Dir, err)
		}
	}
}
