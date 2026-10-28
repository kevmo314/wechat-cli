package main

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kevmo314/wechat-cli/internal/native"
)

type profileSwitchRequest struct{ Profile string }

func (e *profileSwitchRequest) Error() string { return "switch profile to " + e.Profile }
func profileStore(base Store, configRoot, requested string, explicitDir bool) (Store, error) {
	profile, err := native.NormalizeProfile(requested)
	if err != nil {
		return Store{}, err
	}
	slug := profile
	if profile == native.AndroidTabletProfile {
		slug = "tablet"
	}
	if explicitDir {
		state, err := (nativeStore{filepath.Join(base.Dir, "native")}).state()
		if err != nil {
			return Store{}, err
		}
		if state.Account != nil {
			saved, _ := native.NormalizeProfile(state.Account.Profile)
			if saved == profile {
				return base, nil
			}
		}
		child := Store{filepath.Join(base.Dir, "profiles", slug)}
		childState, err := (nativeStore{filepath.Join(child.Dir, "native")}).state()
		if err != nil {
			return Store{}, err
		}
		if childState.Account != nil {
			saved, _ := native.NormalizeProfile(childState.Account.Profile)
			if saved != profile {
				return Store{}, fmt.Errorf("profile directory contains a different account type")
			}
			return child, nil
		}
		if state.Account == nil {
			return base, nil
		}
		return child, nil
	}
	target := Store{filepath.Join(base.Dir, "profiles", slug)}
	candidates := []Store{target, base}
	if profile == native.AndroidTabletProfile {
		candidates = append(candidates, Store{filepath.Join(configRoot, "wechat-tablet")})
	}
	var matches []Store
	for _, candidate := range candidates {
		state, err := (nativeStore{filepath.Join(candidate.Dir, "native")}).state()
		if err != nil {
			if candidate.Dir == target.Dir {
				return Store{}, err
			}
			continue
		}
		if state.Account != nil {
			saved, _ := native.NormalizeProfile(state.Account.Profile)
			if saved == profile {
				matches = append(matches, candidate)
			}
		}
	}
	if len(matches) > 1 {
		paths := make([]string, len(matches))
		for i, match := range matches {
			paths[i] = fmt.Sprintf("%q", match.Dir)
		}
		return Store{}, fmt.Errorf("multiple saved %s profiles found in %s; use --config-dir DIR to select one explicitly", profile, strings.Join(paths, ", "))
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	return target, nil
}
func loginProfileArgument(args []string) (string, bool, error) {
	opts, err := parseNativeLoginOptions(args)
	return opts.profile, opts.profileExplicit, err
}

type profilePreference struct {
	Profile string `json:"profile"`
}

func preferredProfileStore(base Store, configRoot string, custom ...bool) (Store, error) {
	explicit := len(custom) > 0 && custom[0]
	var preference profilePreference
	if err := (nativeStore{base.Dir}).load("preferences.json", &preference); err == nil && preference.Profile != "" {
		selected, err := profileStore(base, configRoot, preference.Profile, explicit)
		if err != nil {
			return Store{}, err
		}
		state, err := (nativeStore{filepath.Join(selected.Dir, "native")}).state()
		if err != nil {
			return Store{}, err
		}
		if state.Account != nil {
			return selected, nil
		}
	}
	if explicit {
		return base, nil
	}
	tablet, err := profileStore(base, configRoot, native.AndroidTabletProfile, false)
	if err != nil {
		return Store{}, err
	}
	state, err := (nativeStore{filepath.Join(tablet.Dir, "native")}).state()
	if err != nil {
		return Store{}, err
	}
	if state.Account != nil {
		return tablet, nil
	}
	state, err = (nativeStore{filepath.Join(base.Dir, "native")}).state()
	if err != nil {
		return Store{}, err
	}
	if state.Account != nil {
		return profileStore(base, configRoot, state.Account.Profile, false)
	}
	desktop, err := profileStore(base, configRoot, native.DesktopProfile, false)
	if err != nil {
		return Store{}, err
	}
	state, err = (nativeStore{filepath.Join(desktop.Dir, "native")}).state()
	if err != nil {
		return Store{}, err
	}
	if state.Account != nil {
		return desktop, nil
	}
	return base, nil
}
func rememberProfile(base Store, profile string) error {
	if err := base.init(); err != nil {
		return err
	}
	return (nativeStore{base.Dir}).save("preferences.json", profilePreference{Profile: profile})
}
