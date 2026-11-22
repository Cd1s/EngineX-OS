//go:build !linux

package mapping

import "errors"

// OpenSource needs the Linux ALSA sequencer.
func OpenSource() (Source, error) { return nil, errors.New("the ALSA sequencer is Linux only") }
