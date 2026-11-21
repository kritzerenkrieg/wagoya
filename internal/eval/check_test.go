package eval

import "testing"

// F7: ownPhotoCheck's own regression cases, run on every go test (no model).
func TestOwnPhotoCheck(t *testing.T) {
	for text, ok := range map[string]bool{
		"That's a simple drawing of a beach: sea, sand and a yellow sun.":                                                  true,
		"This is a beach illustration. I can't see obvious signs of editing, but it looks drawn rather than photographed.": true,
		"A beach drawing. I don't see any signs it was edited.":                                                            true,
		"This is a beach. I can't see any photo editing.":                                                                  true,
		"A beach scene. I can't see any image manipulation here.":                                                          true,
		"I can't see the beach photo. Anthropic's tool-calling claim is correct.":                                          false,
		"I couldn't open the attached beach image, but a beach sounds nice.":                                               false,
		"Beach? I can't see the photo you sent.":                                                                           false,
		"Anthropic's 37% claim is real. Nice beach pic though.":                                                            false,
		"Not confirmed. I searched and couldn't find Anthropic's 37% figure.":                                              false,
	} {
		if err := ownPhotoCheck(text); (err == nil) != ok {
			t.Errorf("%q: %v, want ok=%v", text, err, ok)
		}
	}
}
