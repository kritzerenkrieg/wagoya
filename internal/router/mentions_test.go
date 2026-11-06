package router

import "testing"

func TestResolveMentions(t *testing.T) {
	handles := map[string]string{"P1": "143", "P2": "pn:6281", "P3": "999"}
	names := map[string]string{"P1": "John Doe", "P2": "Omar", "P3": "Ana Maria"}
	send, stored, ms := ResolveMentions("@P2 ready? @P1 too. @P9 no. @P2 again", handles, names)
	if send != "@6281 ready? @143 too. @P9 no. @6281 again" || stored != "@Omar ready? @John Doe too. @P9 no. @Omar again" {
		t.Fatalf("send %q stored %q", send, stored)
	}
	if len(ms) != 2 || ms[0].Key != "pn:6281" || ms[1].User != "143" {
		t.Fatalf("mentions %+v", ms)
	}
	send, _, ms = ResolveMentions("there you go, @john doe.", handles, names)
	if send != "there you go, @143." || len(ms) != 1 {
		t.Fatalf("name tag: %q %+v", send, ms)
	}
	if send, _, ms := ResolveMentions("no tags, mail me@example.com", handles, names); send != "no tags, mail me@example.com" || ms != nil {
		t.Fatalf("plain: %q %+v", send, ms)
	}
}
