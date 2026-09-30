package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"strings"
)

// Request-local counterpart of Codex local3's overflow ladder. Only a failed
// identical input enables it; no transcripts or images are persisted.
type imageLadderState struct {
	Fingerprint [32]byte
	Tier        int
	Pending     bool
	Eligible    bool
	Heavy       bool
	InputItems  int
}

var ladderPlaceholder = func() string {
	var b bytes.Buffer
	_ = png.Encode(&b, image.NewNRGBA(image.Rect(0, 0, 1, 1)))
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(b.Bytes())
}()

func imageSlots(input []any) []map[string]any {
	var slots []map[string]any
	for _, v := range input {
		item, _ := v.(map[string]any)
		key := "content"
		if strings.HasSuffix(str(item, "type"), "_output") {
			key = "output"
		}
		parts, _ := item[key].([]any)
		for _, v := range parts {
			part, _ := v.(map[string]any)
			if str(part, "type") == "input_image" && str(part, "image_url") != "" {
				slots = append(slots, part)
			}
		}
	}
	return slots
}

func applyImageTier(input []any, tier int) int {
	slots := imageSlots(input)
	keep := 5
	if tier == 2 || tier == 4 {
		keep = 1
	}
	changed := 0
	for _, p := range slots[:max(0, len(slots)-keep)] {
		if str(p, "image_url") == ladderPlaceholder {
			continue
		}
		if tier <= 2 {
			if str(p, "detail") != "original" {
				continue
			}
			p["detail"] = "high"
		} else {
			p["image_url"] = ladderPlaceholder
			p["detail"] = "high"
		}
		changed++
	}
	return changed
}

func imageLadderTrigger(e sessionError) bool {
	if e.HTTP == 524 {
		return true
	}
	// A generic 422 (including invalid image/unsupported parameter) is NOT a
	// context error. Permission, authentication and rate errors never shrink input.
	if e.HTTP != 400 && e.HTTP != 413 && e.HTTP != 422 && e.HTTP != 200 {
		return false
	}
	s := strings.ToLower(e.Code + " " + e.Message)
	return strings.Contains(s, "context_length_exceeded") || strings.Contains(s, "context window") || strings.Contains(s, "maximum context length") || strings.Contains(s, "too many tokens")
}

func (r *relay) prepareImageLadder(req *http.Request, raw []byte, key string, seq uint64, bps bool) *http.Request {
	if key == "" || responsesPath(req.URL.Path) != "/responses" {
		return req
	}
	var src map[string]any
	if json.Unmarshal(raw, &src) != nil {
		return req
	}
	input, ok := src["input"].([]any)
	if !ok {
		return req
	}
	canonical, _ := json.Marshal(input)
	fingerprint := sha256.Sum256(append([]byte(fmt.Sprintf("%t/%s/", bps, str(src, "model"))), canonical...))
	slots := imageSlots(input)
	r.mu.Lock()
	s := r.sessions[key]
	if s == nil || s.sequence != seq {
		r.mu.Unlock()
		return req
	}
	state := &s.ImageLadder
	if state.Fingerprint != fingerprint {
		carryTier := 0
		// Continue a successful recovery when the client merely appends history.
		// A changed prefix/model/route or compacted history starts fresh. Hashes
		// prove continuity without retaining any text or image bytes in state.
		if state.Tier > 0 && state.InputItems > 0 && len(input) > state.InputItems {
			prefix, _ := json.Marshal(input[:state.InputItems])
			previous := sha256.Sum256(append([]byte(fmt.Sprintf("%t/%s/", bps, str(src, "model"))), prefix...))
			if previous == state.Fingerprint {
				carryTier = state.Tier
			}
		}
		*state = imageLadderState{Fingerprint: fingerprint, Tier: carryTier, InputItems: len(input), Eligible: len(slots) > 1, Heavy: len(slots) > 5 && len(canonical) >= 4<<20}
		s.ImageOptimization = ""
	}
	tier := state.Tier
	if state.Pending {
		state.Pending = false
		for tier < 4 {
			tier++
			if applyImageTier(input, tier) > 0 {
				break
			}
		}
		state.Tier = tier
	}
	changed := 0
	// Reconstruct from original input, so reports count the entire active tier.
	if tier > 0 {
		_ = json.Unmarshal(canonical, &input)
		changed = applyImageTier(input, tier)
	}
	if changed > 0 {
		action := "降低旧图细节"
		if tier >= 3 {
			action = "旧图替换为占位图"
		}
		keep := 5
		if tier == 2 || tier == 4 {
			keep = 1
		}
		s.ImageOptimization = fmt.Sprintf("图片减负第 %d 级：%s %d 张；最近 %d 张不变；原会话文件不变", tier, action, changed, keep)
	}
	r.mu.Unlock()
	if changed == 0 {
		return req
	}
	src["input"] = input
	// Explicitly disclose omitted visual evidence to the receiving model.
	if tier >= 3 {
		src["input"] = append([]any{developerMessage("Image-load recovery: older images have been replaced with 1x1 placeholders after an upstream context/timeout error. Text and the newest images are preserved. Do not infer details from placeholder images; request the original image again when needed.")}, input...)
	}
	body, err := json.Marshal(src)
	if err != nil {
		return req
	}
	clone := req.Clone(req.Context())
	clone.Body = io.NopCloser(bytes.NewReader(body))
	clone.ContentLength = int64(len(body))
	clone.Header.Del("Content-Length")
	clone.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	return clone
}
