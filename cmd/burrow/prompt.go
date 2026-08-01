// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
)

// prompter reads a sequence of answers from one buffered reader over the command's stdin. The
// buffering is why it exists: a fresh bufio.Reader per question discards whatever the previous read
// buffered, which silently swallows the second answer when a command asks more than once (the
// picker, then the agent-wiring offer). It writes its prompts to the same writer the command prints
// to, so a piped transcript reads in order.
type prompter struct {
	r *bufio.Reader
	w io.Writer
}

func newPrompter(in io.Reader, out io.Writer) *prompter {
	return &prompter{r: bufio.NewReader(in), w: out}
}

// line prints prompt and reads one line, trimmed. EOF with no input yields an empty line and no
// error, so a caller's default applies rather than the read failing.
func (p *prompter) line(prompt string) (string, error) {
	fmt.Fprint(p.w, prompt)
	s, err := p.r.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("reading input: %w", err)
	}
	return strings.TrimSpace(s), nil
}

// confirm asks a yes/no question and returns def for an empty line or EOF, so the capitalized letter
// in a "(Y/n)" or "(y/N)" prompt is what pressing return actually does. Anything but an explicit
// y/yes or n/no also falls back to the default rather than looping: a confirmation is not worth
// interrogating someone over.
func (p *prompter) confirm(prompt string, def bool) (bool, error) {
	answer, err := p.line(prompt)
	if err != nil {
		return false, err
	}
	switch strings.ToLower(answer) {
	case "y", "yes":
		return true, nil
	case "n", "no":
		return false, nil
	default:
		return def, nil
	}
}
