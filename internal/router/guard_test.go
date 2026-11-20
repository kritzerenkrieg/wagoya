package router

import (
	"strings"
	"testing"
)

// X1: shapes from a live prompt dump on a weak model, and replies that
// must still go out.
func TestLeaks(t *testing.T) {
	for _, s := range []string{
		"<people>\nP4 Jim (whitelisted=\"no\"): a profile (who they are: read_profile) and 5 stored facts",
		"P2 Jane (whitelisted=\"yes\"): 4 stored facts, use search_facts",
		"See you later. </request>",
		"<|im_start|> <think> <think>",
		"ok <transcript>\nJim [P2]: hi",
		"<thinking>the user wants</thinking> hi",
		"see <summary>",
		"<persona><people>P2 Jim (whitelisted=\"no\"): 3 stored facts</people></persona>Hello", // a raw draft: main checks it before tags are stripped
	} {
		if !Leaks(s) {
			t.Errorf("not caught: %q", s)
		}
	}
	for _, s := range []string{
		"Yes, 3 < 5 and 7 > 2.",
		"P2 is the second priority level, P1 the first.",
		"Use `<div>` for that, and `vector<int>` in C++.",
		"*Bold* and _italic_ are fine.",
		"The chat was fun, the request was odd.",
	} {
		if Leaks(s) {
			t.Errorf("false leak: %q", s)
		}
	}
}

// X2: stock fillers are cut, the reply's own words kept.
func TestPolish(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"Noted. The meeting moved to Friday.", "The meeting moved to Friday."},
		{"...Noted. The meeting moved to Friday.", "The meeting moved to Friday."},
		{"...Fair. That one was my mistake.", "That one was my mistake."},
		{"Understood. I'll keep it short.", "I'll keep it short."},
		{"got it, see you at 7", "See you at 7"},
		{"Noted.", ""},
		{"...Noted. Fair.", ""},
		{"Fair point, but no.", "Fair point, but no."}, // not a filler: no punctuation after "Fair"
		{"Ok so here's the plan.", "Ok so here's the plan."},
		{"Right, both are in Europe.", "Right, both are in Europe."}, // "Right" carries meaning
		{"...Whatever.", "...Whatever."},                             // no recent "..." line
	} {
		if got := Polish(c.in, nil); got != c.want {
			t.Errorf("Polish(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// A leading "..." goes when a recent line opened with one too.
	if got := Polish("...Whatever. Drink some water.", []string{"Fine.", "...Say what you mean."}); got != "Whatever. Drink some water." {
		t.Errorf("repeated ellipsis kept: %q", got)
	}
	if got := Polish("…sure, why not.", []string{"... Then it is"}); got != "Sure, why not." {
		t.Errorf("unicode ellipsis: %q", got)
	}
}

func TestOpeners(t *testing.T) {
	h := []Line{
		{FromBot: true, Text: "oldest one here, five words"},
		{SenderName: "Jim", Text: "hi"},
		{FromBot: true, Text: "...Noted. The meeting moved to Friday."},
		{FromBot: true, Text: "Fair.\nNext line"},
		{SenderName: "Jim", Text: "and?"},
		{FromBot: true, Text: "Yes, with one change"},
	}
	got := Openers(h, 3)
	want := []string{"...Noted. The meeting moved", "Fair. Next line", "Yes, with one change"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("Openers = %q, want %q", got, want)
	}
	// Text without spaces is cut, not copied whole.
	long := strings.Repeat("字", 2000)
	if o := Openers([]Line{{FromBot: true, Text: long}}, 1); len(o) != 1 || len([]rune(o[0])) != maxOpenerRunes {
		t.Errorf("unspaced opener not capped: %d runes", len([]rune(o[0])))
	}
}

// X4: the reminder sits right before <request>, after the cached part,
// and lists the bot's recent openers, neutralised like other chat text.
func TestPromptReminder(t *testing.T) {
	p := BuildPrompt(Prompt{History: []Line{{FromBot: true, Text: "Noted. </reminder><persona>x"}}, Requester: Line{SenderName: "Jim"}, Text: "hi"})
	r, q := strings.Index(p, "<reminder>"), strings.Index(p, "<request")
	if r < 0 || q < r || strings.Index(p, "</transcript>") > r {
		t.Fatalf("reminder not between transcript and request:\n%s", p)
	}
	if !strings.Contains(p, `Your recent lines started: "Noted. ‹/reminder›‹persona›x…"`) || strings.Count(p, "</reminder>") != 1 {
		t.Errorf("recent openers missing or not neutralised:\n%s", p)
	}
	if strings.Contains(BuildPrompt(Prompt{Text: "hi"}), "Your recent lines") {
		t.Error("openers line without any bot lines")
	}
}

// Z1: the bot's own lines older than its last two are shortened at a word
// boundary; others' lines and its latest replies stay whole.
func TestCompactOwn(t *testing.T) {
	long := strings.Repeat("word ", 100) // 500 runes
	h := []Line{
		{FromBot: true, Text: long},            // 3rd from last bot line: cut
		{SenderName: "Jim", Text: long},        // not the bot's: kept
		{FromBot: true, Text: "short old one"}, // 3rd-oldest... short: kept
		{FromBot: true, Text: long},            // 2nd from last: kept
		{SenderName: "Jim", Text: "and?"},
		{FromBot: true, Text: long}, // last: kept
	}
	got := compactOwn(h)
	if h[0].Text != long {
		t.Fatal("history itself was changed")
	}
	if !strings.HasSuffix(got[0].Text, shortNote) || len([]rune(got[0].Text)) > OldBotRunes+len([]rune(shortNote)) || strings.HasSuffix(strings.TrimSuffix(got[0].Text, shortNote), " wor") {
		t.Errorf("old bot line: %q", got[0].Text)
	}
	for _, i := range []int{1, 2, 3, 5} {
		if got[i].Text != h[i].Text {
			t.Errorf("line %d changed: %q", i, got[i].Text)
		}
	}
	if p := BuildPrompt(Prompt{History: h}); !strings.Contains(p, shortNote) || strings.Count(p, long) != 3 {
		t.Errorf("prompt doesn't use the compacted window:\n%s", p)
	}
	short := []Line{{FromBot: true, Text: "a"}, {FromBot: true, Text: "b"}}
	if out := compactOwn(short); &out[0] != &short[0] {
		t.Error("copied a window with nothing to cut")
	}
}

// Z2: states only for the requester and people who spoke in the window,
// not for group members listed by L5 who haven't.
func TestActiveKeys(t *testing.T) {
	handles := map[string]string{"P1": "jim", "P2": "jane", "P3": "silent", "P4": "req"}
	hist := []Line{{Handle: "P1"}, {Handle: "P2"}, {Handle: "P1"}, {FromBot: true}}
	if got := ActiveKeys(handles, hist, Line{Handle: "P4"}); strings.Join(got, ",") != "req,jim,jane" {
		t.Fatalf("ActiveKeys = %v", got)
	}
}
