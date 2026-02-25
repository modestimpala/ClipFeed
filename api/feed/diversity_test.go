package feed

import "testing"

// strPtr returns a pointer to s, making it easy to build *string map values
// that match the type assertions inside applyDiversityPenalty.
func strPtr(s string) *string { return &s }

func TestApplyDiversityPenalty_SingleClip_IsNoOp(t *testing.T) {
	clips := []map[string]interface{}{
		{"id": "a", "_score": 1.0, "topics": []string{"tech"}},
	}
	(&Handler{}).applyDiversityPenalty(clips, 1.0)
	if clips[0]["id"] != "a" {
		t.Error("single clip changed identity")
	}
	if _, ok := clips[0]["_div_score"]; ok {
		t.Error("_div_score should be cleaned up after single-clip run")
	}
}

func TestApplyDiversityPenalty_DivScoreNotLeaked(t *testing.T) {
	clips := []map[string]interface{}{
		{"id": "a", "_score": 2.0},
		{"id": "b", "_score": 1.0},
	}
	(&Handler{}).applyDiversityPenalty(clips, 0.5)
	for _, c := range clips {
		if _, ok := c["_div_score"]; ok {
			t.Errorf("_div_score leaked in result for clip %v", c["id"])
		}
	}
}

func TestApplyDiversityPenalty_ZeroMixPreservesOrder(t *testing.T) {
	// diversityMix=0 means topicDecay=1.0, channelDecay=1.0, platformDecay=1.0,
	// so all penalty multipliers are 1.0 and the greedy pass is order-preserving.
	ch := strPtr("chan-x")
	clips := []map[string]interface{}{
		{"id": "a", "_score": 3.0, "topics": []string{"tech"}, "channel_name": ch},
		{"id": "b", "_score": 2.0, "topics": []string{"tech"}, "channel_name": ch},
		{"id": "c", "_score": 1.0, "topics": []string{"tech"}, "channel_name": ch},
	}
	(&Handler{}).applyDiversityPenalty(clips, 0.0)
	want := []string{"a", "b", "c"}
	for i, wantID := range want {
		if got, _ := clips[i]["id"].(string); got != wantID {
			t.Errorf("pos %d: got %q, want %q", i, got, wantID)
		}
	}
}

func TestApplyDiversityPenalty_SameTopicPenalizes(t *testing.T) {
	// Clips a and b share "tech"; c has "sports" at a slightly lower base score.
	// After the first "tech" pick (a), the topic penalty on b should let c rise above it.
	chA := strPtr("chan-a")
	chB := strPtr("chan-b")
	clips := []map[string]interface{}{
		{"id": "a", "_score": 3.0, "topics": []string{"tech"}, "channel_name": chA},
		{"id": "b", "_score": 2.0, "topics": []string{"tech"}, "channel_name": chB},
		{"id": "c", "_score": 1.8, "topics": []string{"sports"}},
	}
	(&Handler{}).applyDiversityPenalty(clips, 1.0)

	posB, posC := -1, -1
	for i, cl := range clips {
		switch cl["id"] {
		case "b":
			posB = i
		case "c":
			posC = i
		}
	}
	if posB == -1 || posC == -1 {
		t.Fatal("clip 'b' or 'c' missing from results")
	}
	if posC > posB {
		t.Errorf("expected sports clip 'c' before repeat-topic clip 'b' (posC=%d posB=%d)", posC, posB)
	}
}

func TestApplyDiversityPenalty_SameChannelPenalizes(t *testing.T) {
	// Clips a and b share the same channel; c is from a different channel at a
	// slightly lower base score. High diversity should boost c above b.
	ch := strPtr("same-channel")
	clips := []map[string]interface{}{
		{"id": "a", "_score": 3.0, "channel_name": ch},
		{"id": "b", "_score": 2.0, "channel_name": ch},
		{"id": "c", "_score": 1.9, "channel_name": strPtr("other-channel")},
	}
	(&Handler{}).applyDiversityPenalty(clips, 1.0)

	posB, posC := -1, -1
	for i, cl := range clips {
		switch cl["id"] {
		case "b":
			posB = i
		case "c":
			posC = i
		}
	}
	if posB == -1 || posC == -1 {
		t.Fatal("clip 'b' or 'c' missing from results")
	}
	if posC > posB {
		t.Errorf("expected different-channel clip 'c' before same-channel repeat 'b' (posC=%d posB=%d)", posC, posB)
	}
}

func TestApplyDiversityPenalty_LTRScoreUsedWhenPresent(t *testing.T) {
	// When _l2r_score is present it should take precedence over _score.
	// a has low _score but high _l2r_score → should still win.
	clips := []map[string]interface{}{
		{"id": "a", "_score": 0.1, "_l2r_score": 10.0},
		{"id": "b", "_score": 5.0, "_l2r_score": 1.0},
	}
	(&Handler{}).applyDiversityPenalty(clips, 0.0)
	if got, _ := clips[0]["id"].(string); got != "a" {
		t.Errorf("pos 0 = %q, want 'a' (highest _l2r_score should win)", got)
	}
}
