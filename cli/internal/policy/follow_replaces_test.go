/*
Copyright 2026 Google Inc. All Rights Reserved.
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package policy

import (
	"flag"
	"io"
	"testing"

	"github.com/google/googet/v2/settings"
)

// TestFollowReplacesPrecedence checks that FollowReplaces uses the
// -follow_replaces flag when it was passed, in any spelling, and otherwise
// settings.FollowReplaces, and that -force has no say.
func TestFollowReplacesPrecedence(t *testing.T) {
	t.Cleanup(settings.Snapshot())
	for _, tc := range []struct {
		name string
		args []string
		// want maps settings.FollowReplaces to the expected result.
		want map[bool]bool
	}{
		{name: "unset", args: nil, want: map[bool]bool{false: false, true: true}},
		{name: "bare", args: []string{"-follow_replaces"}, want: map[bool]bool{false: true, true: true}},
		{name: "true", args: []string{"-follow_replaces=true"}, want: map[bool]bool{false: true, true: true}},
		{name: "false", args: []string{"-follow_replaces=false"}, want: map[bool]bool{false: false, true: false}},
		{name: "1", args: []string{"-follow_replaces=1"}, want: map[bool]bool{false: true, true: true}},
		{name: "0", args: []string{"-follow_replaces=0"}, want: map[bool]bool{false: false, true: false}},
		{name: "last wins", args: []string{"-follow_replaces", "-follow_replaces=false"}, want: map[bool]bool{false: false, true: false}},
	} {
		for _, conf := range []bool{false, true} {
			for _, force := range []bool{false, true} {
				settings.FollowReplaces = conf
				var fl Flags
				fs := flag.NewFlagSet("update", flag.ContinueOnError)
				fs.SetOutput(io.Discard)
				fl.Register(fs)
				fl.RegisterFollowReplaces(fs)
				args := tc.args
				if force {
					fs.Bool("force", false, "")
					args = append([]string{"-force"}, args...)
				}
				if err := fs.Parse(args); err != nil {
					t.Fatalf("%s: Parse(%q): %v", tc.name, args, err)
				}
				_ = fl.Options(false, force)
				if got := fl.FollowReplaces(); got != tc.want[conf] {
					t.Errorf("%s: conf=%t force=%t: FollowReplaces() = %t, want %t", tc.name, conf, force, got, tc.want[conf])
				}
			}
		}
	}
}

// TestFollowReplacesZeroValueAndUnregistered checks that a zero Flags, and
// one whose -follow_replaces flag was never registered, fall back to
// settings.FollowReplaces.
func TestFollowReplacesZeroValueAndUnregistered(t *testing.T) {
	t.Cleanup(settings.Snapshot())
	for _, conf := range []bool{false, true} {
		settings.FollowReplaces = conf
		var zero Flags
		if got := zero.FollowReplaces(); got != conf {
			t.Errorf("zero Flags: FollowReplaces() = %t, want %t", got, conf)
		}
		var fl Flags
		fs := flag.NewFlagSet("install", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		fl.Register(fs)
		if fs.Lookup("follow_replaces") != nil {
			t.Error("Register bound -follow_replaces, want it bound only by RegisterFollowReplaces")
		}
		if err := fs.Parse([]string{"-follow_replaces"}); err == nil {
			t.Error("Parse(-follow_replaces) without RegisterFollowReplaces succeeded, want error")
		}
		if got := fl.FollowReplaces(); got != conf {
			t.Errorf("unregistered: FollowReplaces() = %t, want %t", got, conf)
		}
	}
}

// TestFollowReplacesFlagRejectsInvalidValues checks that empty, padded and
// unknown -follow_replaces values fail parsing with an error naming the flag
// and the value, and leave the setting in charge.
func TestFollowReplacesFlagRejectsInvalidValues(t *testing.T) {
	t.Cleanup(settings.Snapshot())
	settings.FollowReplaces = true
	for _, bad := range []string{"", " ", " true", "true ", "maybe", "yes", "on", "2"} {
		var fl Flags
		fs := flag.NewFlagSet("update", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		fl.RegisterFollowReplaces(fs)
		err := fs.Parse([]string{"-follow_replaces=" + bad})
		want := `invalid boolean value "` + bad + `" for -follow_replaces: want true or false`
		if err == nil || err.Error() != want {
			t.Errorf("Parse(-follow_replaces=%q) = %v, want %q", bad, err, want)
		}
		if !fl.FollowReplaces() {
			t.Errorf("after rejected value %q, FollowReplaces() = false, want true from settings", bad)
		}
	}
}

// TestFollowReplacesFlagValue checks the flag.Value contract of the
// -follow_replaces flag: it is a boolean flag whose default prints as false
// and whose String reflects the parsed value.
func TestFollowReplacesFlagValue(t *testing.T) {
	var fl Flags
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fl.RegisterFollowReplaces(fs)
	f := fs.Lookup("follow_replaces")
	if f == nil {
		t.Fatal("RegisterFollowReplaces did not bind -follow_replaces")
	}
	if f.DefValue != "false" {
		t.Errorf("DefValue = %q, want false", f.DefValue)
	}
	if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); !ok || !bf.IsBoolFlag() {
		t.Error("-follow_replaces is not a boolean flag")
	}
	if err := fs.Parse([]string{"-follow_replaces"}); err != nil {
		t.Fatal(err)
	}
	if got := f.Value.String(); got != "true" {
		t.Errorf("String() after -follow_replaces = %q, want true", got)
	}
	var nilFlag *boolFlag
	if got := nilFlag.String(); got != "false" {
		t.Errorf("nil boolFlag String() = %q, want false", got)
	}
}
