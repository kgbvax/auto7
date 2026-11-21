// SPDX-License-Identifier: AGPL-3.0-or-later

package hunt

import (
	"testing"
)

func TestEtiquette(t *testing.T) {
	const own = "XX9ZZ"
	cases := []struct {
		text                       string
		callable, otherQSO, caller bool
	}{
		{"CQ XX1AA AA00", true, false, false},
		{"CQ DX XX1AA AA00", true, false, false},
		{"XY4DD XX1AA RR73", true, false, false},
		{"XY4DD XX1AA RRR", true, false, false},
		{"XY4DD XX1AA 73", true, false, false},
		{"XY4DD XX1AA -10", false, true, false},
		{"XY4DD XX1AA R-10", false, true, false},
		{"XY4DD XX1AA AA00", false, true, false},
		{"XY4DD XX1AA", false, true, false},
		{"XX9ZZ XX1AA AA00", false, false, true},
		{"XX9ZZ XX1AA -10", false, false, true},
		{"XX9ZZ XX1AA", false, false, true},
		{"XX9ZZ XX1AA RR73", false, false, false},
		{"XX9ZZ XX1AA 73", false, false, false},
	}
	for _, c := range cases {
		m, ok := ParseMessage(c.text)
		if !ok {
			t.Fatalf("ParseMessage(%q) failed", c.text)
		}
		s := Station{Call: m.From, To: m.To, LastMsg: m.Type}
		if got := callable(s, own); got != c.callable {
			t.Errorf("callable(%q) = %v, want %v", c.text, got, c.callable)
		}
		if got := inOtherQSO(s, own); got != c.otherQSO {
			t.Errorf("inOtherQSO(%q) = %v, want %v", c.text, got, c.otherQSO)
		}
		if got := isCaller(s, own); got != c.caller {
			t.Errorf("isCaller(%q) = %v, want %v", c.text, got, c.caller)
		}
	}
}
