package cmd

import "errors"

const defaultDeclaredDetach = "C-] d"

// declaredDetachDecoder recognizes a control byte followed by one literal byte.
// It buffers at most one byte, including when the chord spans terminal reads.
// This is deliberately separate from the legacy instance's single-byte escape.
type declaredDetachDecoder struct {
	control byte
	literal byte
	pending bool
}

func newDeclaredDetachDecoder(spec string) (*declaredDetachDecoder, error) {
	if len(spec) != 5 || spec[0] != 'C' || spec[1] != '-' || spec[3] != ' ' || spec[4] < '!' || spec[4] > '~' {
		return nil, errors.New("detach keys must be a control key and one literal, for example C-] d")
	}
	key := spec[2]
	if key >= 'a' && key <= 'z' {
		key -= 'a' - 'A'
	}
	var control byte
	switch {
	case key >= '@' && key <= '_':
		control = key - '@'
	case key == '?':
		control = 0x7f
	default:
		return nil, errors.New("detach control key must be an ASCII control-key spelling")
	}
	return &declaredDetachDecoder{control: control, literal: spec[4]}, nil
}

// Feed returns input preceding the first complete detach chord. The chord and
// all following bytes from the same read are never forwarded to the harness.
func (d *declaredDetachDecoder) Feed(input []byte) ([]byte, bool) {
	output := make([]byte, 0, len(input)+1)
	for _, value := range input {
		if d.pending {
			d.pending = false
			if value == d.literal {
				return output, true
			}
			output = append(output, d.control)
		}
		if value == d.control {
			d.pending = true
		} else {
			output = append(output, value)
		}
	}
	return output, false
}

// Flush is used only for a genuine stdin EOF, not cancellation or detach.
func (d *declaredDetachDecoder) Flush() []byte {
	if !d.pending {
		return nil
	}
	d.pending = false
	return []byte{d.control}
}
