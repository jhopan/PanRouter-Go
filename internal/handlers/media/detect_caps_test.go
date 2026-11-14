package media

import (
	"context"
	json "encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type detectCapsBody struct {
	OK    bool   `json:"ok"`
	Note  string `json:"note"`
	Error string `json:"error"`
	Caps  struct {
		Vision    bool `json:"vision"`
		Reasoning bool `json:"reasoning"`
	} `json:"caps"`
	Detail struct {
		Vision         string  `json:"vision"`
		Reasoning      string  `json:"reasoning"`
		ContextWindow  int     `json:"contextWindow"`
		ThinkingFormat *string `json:"thinkingFormat"`
	} `json:"detail"`
}

// Port-of-upstream test for src/app/api/models/detect-caps/route.js.
func TestHandleDetectCaps(t *testing.T) {
	tests := []struct {
		name             string
		body             string
		reply            func(body string) (int, string)
		wantStatus       int
		wantError        string
		wantOK           bool
		wantVision       bool
		wantReason       bool
		wantVisionDetail string
		wantProbes       int
		wantImageProbes  int
	}{
		{
			// A model that already declares both capabilities is never
			// re-probed for vision: only the text probe runs, and the vision
			// verdict stays the heuristic's.
			name: "declared capabilities are not re-probed",
			body: `{"model":"anthropic/claude-opus-4.6"}`,
			reply: func(body string) (int, string) {
				if strings.Contains(body, "image_url") {
					t.Errorf("vision was re-probed for a model that declares it: %s", body)
				}
				return http.StatusOK, `{"model":"claude-opus-4.6","choices":[{"message":{"content":"hi"}}]}`
			},
			wantStatus:       http.StatusOK,
			wantOK:           true,
			wantVision:       true,
			wantReason:       true,
			wantVisionDetail: "heuristic (known vision model)",
			wantProbes:       1,
			wantImageProbes:  0,
		},
		{
			// Text-only heuristic, the image probe answers as the target: vision
			// is confirmed by the probe.
			name: "image probe confirms vision when the target answers",
			body: `{"model":"deepseek/deepseek-chat"}`,
			reply: func(body string) (int, string) {
				if strings.Contains(body, "image_url") {
					return http.StatusOK, `{"model":"deepseek-chat","choices":[{"message":{"content":"Red"}}]}`
				}
				return http.StatusOK, `{"model":"deepseek-chat","choices":[{"message":{"content":"hi"}}]}`
			},
			wantStatus:       http.StatusOK,
			wantOK:           true,
			wantVision:       true,
			wantVisionDetail: "probe (answered about the image)",
			wantProbes:       2,
			wantImageProbes:  1,
		},
		{
			// The Vision Adapter switching to another model is proof the target
			// cannot see; vision stays false and the detail names the answerer.
			name: "image reply from another model is a combo switch",
			body: `{"model":"deepseek/deepseek-chat"}`,
			reply: func(body string) (int, string) {
				if strings.Contains(body, "image_url") {
					return http.StatusOK, `{"model":"gemini-3.8-flash","choices":[{"message":{"content":"Red"}}]}`
				}
				return http.StatusOK, `{"model":"deepseek-chat","choices":[{"message":{"content":"hi"}}]}`
			},
			wantStatus:       http.StatusOK,
			wantOK:           true,
			wantVision:       false,
			wantVisionDetail: "combo-switched (answered by gemini-3.8-flash, not the target)",
			wantProbes:       2,
			wantImageProbes:  1,
		},
		{
			name:       "an id without an alias is refused",
			body:       `{"model":"gpt-5.5"}`,
			wantStatus: http.StatusBadRequest,
			wantError:  "Model required (alias/model-id)",
			wantProbes: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			probes, imageProbes := 0, 0
			h := &MediaHandler{}
			if tt.reply != nil {
				h.chatProbe = func(ctx context.Context, body []byte) (int, []byte) {
					probes++
					payload := string(body)
					if strings.Contains(payload, "image_url") {
						imageProbes++
					}
					status, reply := tt.reply(payload)
					return status, []byte(reply)
				}
			}

			req := httptest.NewRequest(http.MethodPost, "/api/models/detect-caps", strings.NewReader(tt.body))
			rec := httptest.NewRecorder()
			h.HandleDetectCaps(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantError != "" {
				var body map[string]any
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				if body["error"] != tt.wantError {
					t.Errorf("error = %v, want %q", body["error"], tt.wantError)
				}
				if probes != 0 {
					t.Errorf("refused request ran %d probes, want 0", probes)
				}
				return
			}

			var body detectCapsBody
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("unmarshal: %v (%s)", err, rec.Body.String())
			}
			if body.OK != tt.wantOK {
				t.Errorf("ok = %v, want %v", body.OK, tt.wantOK)
			}
			if body.Caps.Vision != tt.wantVision {
				t.Errorf("caps.vision = %v, want %v", body.Caps.Vision, tt.wantVision)
			}
			if tt.wantReason && !body.Caps.Reasoning {
				t.Errorf("caps.reasoning = false, want true")
			}
			if body.Detail.Vision == "" {
				t.Error("detail.vision is empty")
			}
			if tt.wantVisionDetail != "" && body.Detail.Vision != tt.wantVisionDetail {
				t.Errorf("detail.vision = %q, want %q", body.Detail.Vision, tt.wantVisionDetail)
			}
			if body.Note == "" {
				t.Error("note is empty")
			}
			if probes != tt.wantProbes {
				t.Errorf("probes = %d, want %d", probes, tt.wantProbes)
			}
			if imageProbes != tt.wantImageProbes {
				t.Errorf("image probes = %d, want %d", imageProbes, tt.wantImageProbes)
			}
		})
	}
}

func TestHandleDetectCaps_ProbeSurfacesThinkingField(t *testing.T) {
	h := &MediaHandler{}
	h.chatProbe = func(ctx context.Context, body []byte) (int, []byte) {
		return http.StatusOK, []byte(`{"model":"deepseek-reasoner","choices":[{"message":{"content":"hi","reasoning_content":"thinking..."}}]}`)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/models/detect-caps",
		strings.NewReader(`{"model":"deepseek/deepseek-reasoner"}`))
	h.HandleDetectCaps(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body detectCapsBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !body.Caps.Reasoning {
		t.Error("caps.reasoning = false, want the probe's thinking field to win")
	}
	if body.Detail.Reasoning != "probe (thinking field in response)" {
		t.Errorf("detail.reasoning = %q", body.Detail.Reasoning)
	}
}
