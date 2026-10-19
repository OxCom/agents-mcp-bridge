package run

import (
	"errors"
	"io"
	"strings"
	"testing"
)

type gotLine struct {
	text    string
	tooLong bool
}

func readAllLines(t *testing.T, in string, max int) []gotLine {
	t.Helper()
	lr := newLineReader(strings.NewReader(in), max)
	var got []gotLine
	for i := 0; i < 100; i++ {
		line, tooLong, err := lr.next()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				t.Fatalf("err = %v, want io.EOF", err)
			}
			if len(line) != 0 {
				t.Fatalf("data %d bytes returned together with EOF", len(line))
			}
			return got
		}
		got = append(got, gotLine{string(line), tooLong})
	}
	t.Fatal("reader did not reach EOF")
	return nil
}

func TestLineReader(t *testing.T) {
	const bufSize = 64 << 10
	exact := strings.Repeat("a", bufSize)

	tests := []struct {
		name string
		in   string
		max  int
		want []gotLine
	}{
		{"unterminated line of exactly the buffer size", exact, 1 << 20, []gotLine{{exact, false}}},
		{"short unterminated line", "abc", 1024, []gotLine{{"abc", false}}},
		{"crlf lines", "one\r\ntwo\r\n", 1024, []gotLine{{"one", false}, {"two", false}}},
		{"oversized line then a normal one", strings.Repeat("x", 5000) + "\nok\n", 100,
			[]gotLine{{"", true}, {"ok", false}}},
		{"oversized unterminated line at EOF", strings.Repeat("x", 5000), 100, []gotLine{{"", true}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := readAllLines(t, tc.in, tc.max)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d lines, want %d", len(got), len(tc.want))
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("line %d: got (len %d, tooLong %v), want (len %d, tooLong %v)",
						i, len(got[i].text), got[i].tooLong, len(tc.want[i].text), tc.want[i].tooLong)
				}
			}
		})
	}
}
