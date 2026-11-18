package router

import "testing"

func TestResolveMentions(t *testing.T) {
	handles := map[string]string{"P1": "143", "P2": "pn:6281", "P3": "999"}
	names := map[string]string{"P1": "John Doe", "P2": "Jack", "P3": "Mary Major"}
	send, stored, ms := ResolveMentions("@P2 ready? @P1 too. @P9 no. @P2 again", handles, names, nil)
	if send != "@6281 ready? @143 too. @P9 no. @6281 again" || stored != "@Jack ready? @John Doe too. @P9 no. @Jack again" {
		t.Fatalf("send %q stored %q", send, stored)
	}
	if len(ms) != 2 || ms[0].Key != "pn:6281" || ms[1].User != "143" {
		t.Fatalf("mentions %+v", ms)
	}
	send, _, ms = ResolveMentions("there you go, @john doe.", handles, names, nil)
	if send != "there you go, @143." || len(ms) != 1 {
		t.Fatalf("name tag: %q %+v", send, ms)
	}
	if send, _, ms := ResolveMentions("no tags, mail me@example.com", handles, names, nil); send != "no tags, mail me@example.com" || ms != nil {
		t.Fatalf("plain: %q %+v", send, ms)
	}
}

// L3: a raw "@<digits>" tag becomes a mention only for a group member.
func TestResolveMentionsByNumber(t *testing.T) {
	handles := map[string]string{"P1": "100000000000001"}
	names := map[string]string{"P1": "John Doe"}
	numbers := map[string]string{"100000000000001": "100000000000001", "15550000001": "100000000000001", "15550000002": "pn:15550000002"}
	send, stored, ms := ResolveMentions("@15550000001 hi, @100000000000001 again, @15550000002 too, @15550000099 unknown, @P1", handles, names, numbers)
	if send != "@100000000000001 hi, @100000000000001 again, @15550000002 too, @15550000099 unknown, @100000000000001" ||
		stored != "@John Doe hi, @John Doe again, @someone too, @15550000099 unknown, @John Doe" {
		t.Fatalf("send %q stored %q", send, stored)
	}
	if len(ms) != 2 || ms[0].Key != "100000000000001" || ms[1].Key != "pn:15550000002" || ms[1].User != "15550000002" {
		t.Fatalf("mentions %+v", ms)
	}
}

// L10: a scheduled message keeps its tags until it is sent.
func TestPinAndExpandMentions(t *testing.T) {
	handles := map[string]string{"P1": "143", "P2": "pn:6281"}
	names := map[string]string{"P1": "John Doe", "P2": "Jack"}
	pinned := PinMentions("@P1 @P2 futsal in 15. @John Doe again. @P7 stays. sneaky @{999}", handles, names)
	if pinned != "@{143} @{pn:6281} futsal in 15. @{143} again. @P7 stays. sneaky @ {999}" {
		t.Fatalf("pinned %q", pinned)
	}
	send, stored, ms := ExpandMentions(pinned, func(k string) string { return map[string]string{"143": "John"}[k] })
	if send != "@143 @6281 futsal in 15. @143 again. @P7 stays. sneaky @ {999}" ||
		stored != "@John @someone futsal in 15. @John again. @P7 stays. sneaky @ {999}" || len(ms) != 2 {
		t.Fatalf("send %q stored %q mentions %+v", send, stored, ms)
	}
}
