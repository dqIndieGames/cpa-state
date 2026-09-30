package main

import (
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// Live upstream 2026-09-29: a valid PNG in user content returns
// 422 / "422: Invalid request body."; identical tool-result bytes complete and
// the model reads R1–R6 and the original Chinese labels correctly.
// Protect content, role, ordering and non-execution, not the envelope wording.
func TestBPSUserImageTransportPreservesInput(t *testing.T) {
	image := map[string]any{"type": "input_image", "image_url": "data:image/png;base64,original", "detail": "original"}
	first := map[string]any{"type": "input_text", "text": "Inspect the attachment; do not modify anything."}
	last := map[string]any{"type": "input_text", "text": "Compare with the next image."}
	original := map[string]any{"type": "message", "role": "user", "content": []any{first, image, last, image}}
	before, _ := json.Marshal(original)
	src := bridgeSource()
	src["input"] = []any{original}
	r := testRelay(t)
	q, err := bridgeRequest(t, r, "12345678-1234-1234-1234-123456789abc", src)
	if err != nil {
		t.Fatal(err)
	}
	x := exchange(q)
	if len(x.Images) != 1 {
		t.Fatal("missing attachment envelope")
	}
	after, _ := json.Marshal(original)
	if string(before) != string(after) {
		t.Fatal("client input mutated")
	}
	for id, a := range x.Images {
		text := a.Message["content"].([]any)
		if a.Message["role"] != "user" || !reflect.DeepEqual(text[0], first) || !reflect.DeepEqual(text[2], last) {
			t.Fatal("user text or role changed")
		}
		output := a.Result["output"].([]any)
		if !reflect.DeepEqual(output[1], image) || !reflect.DeepEqual(output[2], image) {
			t.Fatal("image bytes/detail/order changed")
		}
		if _, found := r.bpsCache.get(x.Scope + id); found {
			t.Fatal("attachment entered executable history")
		}
		if _, err = x.convertTool(a.Call); err == nil {
			t.Fatal("attachment can be emitted as executable tool")
		}
	}
}

func TestBPSUserImagesCompactRestoresOriginal(t *testing.T) {
	original := map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_image", "image_url": "data:image/png;base64,original"}}}
	x := &bpsExchange{Scope: "one"}
	parts := x.userImages(original, 0, map[string]bool{})
	opaque := map[string]any{"type": "compaction", "encrypted_content": "upstream-opaque"}
	for _, retained := range [][]any{parts, {parts[0]}, {parts[1], parts[2]}, {parts[2]}} {
		output := append(append([]any{}, retained...), opaque)
		raw, _ := json.Marshal(map[string]any{"id": "cmp_real_contract", "object": "response.compaction", "output": output})
		resp := &http.Response{Body: io.NopCloser(strings.NewReader(string(raw))), Header: http.Header{}}
		if err := x.compactResponse(resp); err != nil {
			t.Fatal(err)
		}
		var result map[string]any
		json.NewDecoder(resp.Body).Decode(&result)
		if !reflect.DeepEqual(result["output"], []any{original, opaque}) {
			t.Fatal("client did not receive exactly the original message and opaque compaction")
		}
	}
	altered := cloneObject(parts[2].(map[string]any))
	altered["output"] = "modified"
	if _, err := x.restoreUserImages([]any{altered}); err == nil {
		t.Fatal("changed upstream attachment accepted")
	}
}

func TestBPSUserImagesScopedIDsAndTextPassthrough(t *testing.T) {
	user := map[string]any{"role": "user", "content": "hello"}
	x := &bpsExchange{Scope: "account/session"}
	if got := x.userImages(user, 0, map[string]bool{}); len(got) != 1 || !reflect.DeepEqual(got[0], user) {
		t.Fatal("text changed")
	}
	user["content"] = []any{map[string]any{"type": "input_image", "image_url": "same-bytes"}}
	first := x.userImages(user, 1, map[string]bool{})
	id := str(first[1].(map[string]any), "call_id")
	second := x.userImages(user, 2, map[string]bool{})
	if str(second[1].(map[string]any), "call_id") == id {
		t.Fatal("repeated attachment collides")
	}
	collision := x.userImages(user, 1, map[string]bool{id: true})
	if str(collision[1].(map[string]any), "call_id") == id {
		t.Fatal("client call overwritten")
	}
	other := (&bpsExchange{Scope: "other-account/session"}).userImages(user, 1, map[string]bool{})
	if str(other[1].(map[string]any), "call_id") == id {
		t.Fatal("account scope collision")
	}
}
