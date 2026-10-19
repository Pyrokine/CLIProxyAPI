package common

import (
	"errors"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

type patchInputPhase uint8

const (
	patchInputObject patchInputPhase = iota
	patchInputKeyStart
	patchInputKey
	patchInputColon
	patchInputStringStart
	patchInputString
	patchInputClose
	patchInputComplete
)

// ResponsesPatchInput 按调用解码 JSON 包装，只交付完整的 Unicode 字符
// 参数仅允许一个 string 类型的 input 字段
type ResponsesPatchInput struct {
	phase     patchInputPhase
	key       strings.Builder
	input     strings.Builder
	pending   []byte
	high      uint16
	completed bool
	err       error
}

func (d *ResponsesPatchInput) fail() error {
	if d.err == nil {
		d.err = errors.New("invalid apply_patch tool input")
	}
	return d.err
}

func (d *ResponsesPatchInput) Push(fragment string) (string, error) {
	if d.err != nil {
		return "", d.err
	}
	start := d.input.Len()
	for len(fragment) > 0 {
		c := fragment[0]
		if d.phase != patchInputKey && d.phase != patchInputString && (c == ' ' || c == '\t' || c == '\r' || c == '\n') {
			fragment = fragment[1:]
			continue
		}
		switch d.phase {
		case patchInputObject:
			if c != '{' {
				return "", d.fail()
			}
			d.phase = patchInputKeyStart
		case patchInputKeyStart:
			if c != '"' {
				return "", d.fail()
			}
			d.phase = patchInputKey
		case patchInputColon:
			if c != ':' {
				return "", d.fail()
			}
			d.phase = patchInputStringStart
		case patchInputStringStart:
			if c != '"' {
				return "", d.fail()
			}
			d.phase = patchInputString
		case patchInputClose:
			if c != '}' {
				return "", d.fail()
			}
			d.phase = patchInputComplete
		case patchInputComplete:
			return "", d.fail()
		case patchInputKey, patchInputString:
			if len(d.pending) == 0 && c == '"' {
				if d.high != 0 {
					return "", d.fail()
				}
				if d.phase == patchInputKey {
					if d.key.String() != "input" {
						return "", d.fail()
					}
					d.phase = patchInputColon
				} else {
					d.phase = patchInputClose
				}
			} else if err := d.character(c); err != nil {
				return "", err
			}
		}
		fragment = fragment[1:]
	}
	if d.completed && d.input.Len() != start {
		return "", d.fail()
	}
	return d.input.String()[start:], nil
}

func (d *ResponsesPatchInput) character(c byte) error {
	if len(d.pending) == 0 && c != '\\' && c < utf8.RuneSelf {
		if c < 0x20 || d.high != 0 {
			return d.fail()
		}
		d.writeRune(rune(c))
		return nil
	}
	d.pending = append(d.pending, c)
	if d.pending[0] != '\\' {
		if !utf8.FullRune(d.pending) {
			return nil
		}
		r, size := utf8.DecodeRune(d.pending)
		if (r == utf8.RuneError && size == 1) || size != len(d.pending) || d.high != 0 {
			return d.fail()
		}
		d.writeRune(r)
		d.pending = d.pending[:0]
		return nil
	}
	if len(d.pending) < 2 {
		return nil
	}
	if d.pending[1] != 'u' {
		if d.high != 0 {
			return d.fail()
		}
		var r rune
		switch d.pending[1] {
		case '"', '\\', '/':
			r = rune(d.pending[1])
		case 'b':
			r = '\b'
		case 'f':
			r = '\f'
		case 'n':
			r = '\n'
		case 'r':
			r = '\r'
		case 't':
			r = '\t'
		default:
			return d.fail()
		}
		d.writeRune(r)
		d.pending = d.pending[:0]
		return nil
	}
	if len(d.pending) < 6 {
		return nil
	}
	value, err := strconv.ParseUint(string(d.pending[2:]), 16, 16)
	if err != nil {
		return d.fail()
	}
	d.pending = d.pending[:0]
	r := rune(value)
	switch {
	case d.high != 0:
		if r < 0xdc00 || r > 0xdfff {
			return d.fail()
		}
		d.writeRune(utf16.DecodeRune(rune(d.high), r))
		d.high = 0
	case r >= 0xd800 && r <= 0xdbff:
		d.high = uint16(r)
	case r >= 0xdc00 && r <= 0xdfff:
		return d.fail()
	default:
		d.writeRune(r)
	}
	return nil
}

func (d *ResponsesPatchInput) writeRune(r rune) {
	if d.phase == patchInputKey {
		d.key.WriteRune(r)
	} else {
		d.input.WriteRune(r)
	}
}

func (d *ResponsesPatchInput) Finish(arguments string) (string, error) {
	if d.err != nil {
		return "", d.err
	}
	var final ResponsesPatchInput
	if _, err := final.Push(arguments); err != nil || final.phase != patchInputComplete {
		return "", d.fail()
	}
	input := final.Input()
	if !strings.HasPrefix(input, d.Input()) || (d.completed && input != d.Input()) {
		return "", d.fail()
	}
	tail := input[d.input.Len():]
	d.input.WriteString(tail)
	d.phase = patchInputComplete
	d.pending = nil
	d.high = 0
	d.completed = true
	return tail, nil
}

func (d *ResponsesPatchInput) Input() string {
	return d.input.String()
}
