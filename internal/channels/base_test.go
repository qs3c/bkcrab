package channels

import (
	"reflect"
	"testing"
)

func TestFlattenMarkdownTables(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{
			name: "no pipes — passthrough",
			in:   "hello world",
			want: "hello world",
		},
		{
			name: "pipes in prose — passthrough",
			in:   "the regex was /foo|bar/ — not a table",
			want: "the regex was /foo|bar/ — not a table",
		},
		{
			name: "single-pipe line — passthrough (no separator below)",
			in:   "| something | else |\nplain follow-up",
			want: "| something | else |\nplain follow-up",
		},
		{
			name: "2-col table — collapses to label: value",
			in: "| 项 | 内容 |\n" +
				"|---|---|\n" +
				"| 分类 | 功能建议 |\n" +
				"| 严重度 | 中 |",
			want: "项: 内容\n分类: 功能建议\n严重度: 中",
		},
		{
			name: "3-col table — middle-dot join, separator dropped",
			in: "| Name | Type | Severity |\n" +
				"|---|---|---|\n" +
				"| foo | bug | high |\n" +
				"| bar | feature | medium |",
			want: "Name · Type · Severity\n" +
				"foo · bug · high\n" +
				"bar · feature · medium",
		},
		{
			name: "table with surrounding prose preserved",
			in: "Here's the data:\n" +
				"| k | v |\n" +
				"|---|---|\n" +
				"| a | 1 |\n" +
				"\nDone.",
			want: "Here's the data:\nk: v\na: 1\n\nDone.",
		},
		{
			name: "alignment colons in separator still detected",
			in: "| L | C | R |\n" +
				"|:--|:-:|--:|\n" +
				"| a | b | c |",
			want: "L · C · R\na · b · c",
		},
		{
			name: "escaped pipe in cell round-trips as literal",
			in: "| code | meaning |\n" +
				"|---|---|\n" +
				"| `a \\| b` | union |",
			want: "code: meaning\n`a | b`: union",
		},
		{
			name: "header without separator — not a table, passthrough",
			in: "| just | one | row |\n" +
				"and prose follows",
			want: "| just | one | row |\nand prose follows",
		},
		{
			name: "two tables in one message",
			in: "| a | b |\n|---|---|\n| 1 | 2 |\n\n" +
				"| x | y |\n|---|---|\n| 9 | 8 |",
			want: "a: b\n1: 2\n\nx: y\n9: 8",
		},
		{
			name: "short row in 3-col table — padded so columns stay aligned",
			in: "| a | b | c |\n" +
				"|---|---|---|\n" +
				"| 1 | 2 |",
			want: "a · b · c\n1 · 2 · ",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := FlattenMarkdownTables(tc.in)
			if got != tc.want {
				t.Fatalf("got:\n%q\nwant:\n%q", got, tc.want)
			}
		})
	}
}

func TestSplitOutboundText(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "no marker — single bubble",
			in:   "hello world",
			want: []string{"hello world"},
		},
		{
			name: "single split",
			in:   "first\n" + SplitMessageMarker + "\nsecond",
			want: []string{"first", "second"},
		},
		{
			name: "marker without surrounding newlines still splits",
			in:   "first" + SplitMessageMarker + "second",
			want: []string{"first", "second"},
		},
		{
			name: "trailing marker drops empty tail",
			in:   "only one\n" + SplitMessageMarker + "\n",
			want: []string{"only one"},
		},
		{
			name: "consecutive markers don't produce blank bubbles",
			in:   "a\n" + SplitMessageMarker + "\n\n" + SplitMessageMarker + "\nb",
			want: []string{"a", "b"},
		},
		{
			name: "whitespace-only chunks dropped",
			in:   "   \n" + SplitMessageMarker + "\nactual content\n" + SplitMessageMarker + "\n\t",
			want: []string{"actual content"},
		},
		{
			name: "trims per-chunk whitespace",
			in:   "  first  \n" + SplitMessageMarker + "\n\tsecond\n",
			want: []string{"first", "second"},
		},
		{
			name: "empty input returns empty",
			in:   "",
			want: []string{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := SplitOutboundText(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}
