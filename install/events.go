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

package install

import (
	"fmt"

	"github.com/google/logger"
)

// eventSink receives the log and console output that planning an install
// produces, so that planning itself does no I/O.
type eventSink interface {
	// Infof logs an informational message.
	Infof(format string, args ...any)
	// Warning logs a warning.
	Warning(msg string)
	// Error logs an error.
	Error(msg string)
	// Printf writes a message for the user to standard output.
	Printf(format string, args ...any)
}

// liveSink writes events to the logger and standard output.
type liveSink struct{}

func (liveSink) Infof(format string, args ...any)  { logger.Infof(format, args...) }
func (liveSink) Warning(msg string)                { logger.Warning(msg) }
func (liveSink) Error(msg string)                  { logger.Error(msg) }
func (liveSink) Printf(format string, args ...any) { fmt.Printf(format, args...) }

// eventLevel identifies where an event goes and at what level.
type eventLevel string

// The event levels: the three log levels and standard output.
const (
	levelInfo    eventLevel = "INFO"
	levelWarning eventLevel = "WARNING"
	levelError   eventLevel = "ERROR"
	levelStdout  eventLevel = "STDOUT"
)

// event is one recorded log line or console message.
type event struct {
	level eventLevel
	text  string
}

// recordingSink collects events in order instead of emitting them.
type recordingSink struct {
	events []event
}

func (r *recordingSink) Infof(format string, args ...any) {
	r.events = append(r.events, event{levelInfo, fmt.Sprintf(format, args...)})
}

func (r *recordingSink) Warning(msg string) {
	r.events = append(r.events, event{levelWarning, msg})
}

func (r *recordingSink) Error(msg string) {
	r.events = append(r.events, event{levelError, msg})
}

func (r *recordingSink) Printf(format string, args ...any) {
	r.events = append(r.events, event{levelStdout, fmt.Sprintf(format, args...)})
}
