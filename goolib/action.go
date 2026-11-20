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

package goolib

// Action identifies the package lifecycle operation for which GooGet invokes a
// package's install or uninstall command.
type Action int

const (
	// ActionUnspecified indicates legacy execution with GooGet's environment
	// inherited unchanged.
	ActionUnspecified Action = iota
	// ActionInstall indicates a fresh package installation.
	ActionInstall
	// ActionUpgrade indicates an in-place upgrade to a higher version of the
	// same name and architecture.
	ActionUpgrade
	// ActionDowngrade indicates an in-place downgrade to a lower version of the
	// same name and architecture.
	ActionDowngrade
	// ActionReinstall indicates reinstalling the currently installed version of
	// the same name and architecture.
	ActionReinstall
	// ActionRemove indicates an explicit package removal (or removal of one of
	// its reverse dependencies during googet remove).
	ActionRemove
	// ActionReplace indicates uninstalling a package because another package's
	// Replaces entry takes over for it.
	ActionReplace
	// ActionRemoveDependent indicates uninstalling a dependent package cascaded
	// during a package replacement.
	ActionRemoveDependent
	// ActionRecover indicates reinstalling a previously removed package after a
	// replacement fails.
	ActionRecover
)

// String returns the GOOGET_ACTION environment value for a, or "" for
// ActionUnspecified and out-of-range values.
func (a Action) String() string {
	switch a {
	case ActionInstall:
		return "install"
	case ActionUpgrade:
		return "upgrade"
	case ActionDowngrade:
		return "downgrade"
	case ActionReinstall:
		return "reinstall"
	case ActionRemove:
		return "remove"
	case ActionReplace:
		return "replace"
	case ActionRemoveDependent:
		return "remove-dependent"
	case ActionRecover:
		return "recover"
	default:
		return ""
	}
}

// InstallerContext describes why GooGet runs a package's install or uninstall
// command. The zero value means no context, leaving the child command's
// environment unchanged.
type InstallerContext struct {
	// Action is the lifecycle operation triggering the command.
	Action Action
	// Package is the name.arch.version (PkgSpec.String()) of the package whose
	// command runs.
	Package string
	// PreviousVersion is the version of the same name and architecture installed
	// when an install command starts, or empty otherwise.
	PreviousVersion string
	// ReplacedBy is the name.arch.version of the replacing package; it is
	// rendered in the child environment only when Action is ActionReplace.
	ReplacedBy string
}
