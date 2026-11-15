// Kiro conversation canonicalization — port of
// open-sse/translator/concerns/kiroConversation.js (canonicalizeKiroConversation).
//
// CodeWhisperer validates the conversationState envelope structurally, not
// semantically, and answers 400 REQUEST_BODY_INVALID for:
//
//   - two adjacent turns with the same role;
//   - a history that starts with an assistant turn, or a currentMessage that is
//     not a user turn;
//   - a userInputMessage without `origin` or `modelId`;
//   - a toolUse without an adjacent toolResult carrying the same id, or a
//     toolResult whose use is not in the immediately preceding assistant turn;
//   - two toolUses sharing one id;
//   - a toolResult whose content is a single empty text part;
//   - tool specs outside the current message.
//
// One such defect fails the WHOLE request, so every repair below is cheaper than
// the 400: an unpaired call or result is flattened into the turn's text (the model
// still reads it) and everything else is normalized into the shape the gateway
// accepts.
package translator

import (
	"sort"
	"strconv"
	"strings"

	json "encoding/json/v2"
)

// kiroToolIDMaxLength is the gateway's id limit. The tool NAME limits live in
// kiro_tools.go next to the canonicalizer that applies them.
const kiroToolIDMaxLength = 64

// kiroRepairs counts what the canonicalizer changed, in the vocabulary the
// upstream tests assert on.
type kiroRepairs struct {
	MissingResults  int
	OrphanResults   int
	InvalidToolUses int
}

// KiroCanonicalizeConversation turns loose history/currentMessage pairs into the
// strict shape Kiro accepts, and reports whether the result validates.
//
// nameMap maps a source tool name onto the spec-safe name the catalog declared
// (see KiroNormalizeToolSpecs); toolSpecs is the normalized catalog that travels
// on the current message.
func KiroCanonicalizeConversation(history []map[string]any, current map[string]any, modelID string, toolSpecs []any, nameMap map[string]string) (canonHistory []map[string]any, canonCurrent map[string]any, repairs kiroRepairs, valid bool, errs []string) {
	turns := kiroNormalizeTurns(history, current, modelID)
	specNames := kiroSpecNames(toolSpecs)
	usedIDs := map[string]bool{}

	for i := 0; i < len(turns); i += 2 {
		user, _ := turns[i]["userInputMessage"].(map[string]any)
		if i == 0 {
			// A tool result cannot answer anything at the head of a conversation.
			if results := kiroToolResults(user); len(results) > 0 {
				kiroFlattenResults(user, results)
				repairs.OrphanResults += len(results)
				delete(user["userInputMessageContext"].(map[string]any), "toolResults")
				kiroCleanUserContext(user)
			}
		}
		if i+2 >= len(turns) {
			break
		}
		assistant, _ := turns[i+1]["assistantResponseMessage"].(map[string]any)
		nextUser, _ := turns[i+2]["userInputMessage"].(map[string]any)
		if assistant != nil && nextUser != nil {
			kiroReconcileToolPair(assistant, nextUser, i+1, nameMap, specNames, usedIDs, &repairs)
		}
	}

	finalCurrent, _ := turns[len(turns)-1]["userInputMessage"].(map[string]any)
	if finalCurrent == nil {
		finalCurrent = map[string]any{"content": kiroEmptyUserPlaceholder, "modelId": modelID}
		turns = append(turns, map[string]any{"userInputMessage": finalCurrent})
	}
	ctx, _ := finalCurrent["userInputMessageContext"].(map[string]any)
	if ctx == nil && len(toolSpecs) > 0 {
		ctx = map[string]any{}
		finalCurrent["userInputMessageContext"] = ctx
	}
	if len(toolSpecs) > 0 {
		ctx["tools"] = kiroClone(toolSpecs)
	}
	kiroCleanUserContext(finalCurrent)

	finalHistory := turns[:len(turns)-1]
	verdict := kiroValidateConversation(finalHistory, finalCurrent, toolSpecs)
	if !verdict.valid {
		// The pairing could not be repaired structurally: flatten every structured
		// tool block into text and validate again. Losing the structure is far
		// cheaper than losing the turn.
		kiroFlattenAllStructuredTools(turns, &repairs)
		finalCurrent, _ = turns[len(turns)-1]["userInputMessage"].(map[string]any)
		finalHistory = turns[:len(turns)-1]
		verdict = kiroValidateConversation(finalHistory, finalCurrent, toolSpecs)
	}
	return finalHistory, finalCurrent, repairs, verdict.valid, verdict.errors
}

// kiroVerdict is the validation result.
type kiroVerdict struct {
	valid  bool
	errors []string
}

// kiroNormalizeTurns merges adjacent same-role turns, guarantees a user turn at
// both ends, and fills the fields the gateway requires on every turn.
func kiroNormalizeTurns(history []map[string]any, current map[string]any, modelID string) []map[string]any {
	raw := make([]map[string]any, 0, len(history)+1)
	for _, turn := range history {
		if turn != nil {
			raw = append(raw, turn)
		}
	}
	if current != nil {
		raw = append(raw, map[string]any{"userInputMessage": current})
	}

	turns := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		user, isUser := item["userInputMessage"].(map[string]any)
		assistant, isAssistant := item["assistantResponseMessage"].(map[string]any)
		if isUser == isAssistant {
			continue // neither, or both: not a turn
		}
		if isUser {
			turn := map[string]any{"userInputMessage": kiroCloneMap(user)}
			if prev := len(turns) - 1; prev >= 0 {
				if p, ok := turns[prev]["userInputMessage"].(map[string]any); ok {
					kiroMergeUser(p, user)
					continue
				}
			}
			turns = append(turns, turn)
			continue
		}
		turn := map[string]any{"assistantResponseMessage": kiroCloneMap(assistant)}
		if prev := len(turns) - 1; prev >= 0 {
			if p, ok := turns[prev]["assistantResponseMessage"].(map[string]any); ok {
				kiroMergeAssistant(p, assistant)
				continue
			}
		}
		turns = append(turns, turn)
	}

	if len(turns) > 0 {
		if _, startsAssistant := turns[0]["assistantResponseMessage"]; startsAssistant {
			turns = append([]map[string]any{kiroContinueTurn(modelID)}, turns...)
		}
	}
	if len(turns) == 0 {
		turns = append(turns, kiroContinueTurn(modelID))
	} else if _, endsAssistant := turns[len(turns)-1]["assistantResponseMessage"]; endsAssistant {
		turns = append(turns, kiroContinueTurn(modelID))
	}

	for _, turn := range turns {
		if user, ok := turn["userInputMessage"].(map[string]any); ok {
			content := strings.TrimSpace(kiroText(user["content"]))
			if content == "" {
				content = kiroEmptyUserPlaceholder
			}
			user["content"] = content
			if s, _ := user["modelId"].(string); s == "" {
				user["modelId"] = modelID
			}
			// CodeWhisperer rejects conversations whose user turns lack `origin`
			// with 400 REQUEST_BODY_INVALID — every turn in history, not just the
			// current one.
			if s, _ := user["origin"].(string); s == "" {
				user["origin"] = "AI_EDITOR"
			}
			if ctx, ok := user["userInputMessageContext"].(map[string]any); ok {
				// Tool specs belong to the current message only.
				delete(ctx, "tools")
			}
			continue
		}
		if assistant, ok := turn["assistantResponseMessage"].(map[string]any); ok {
			content := strings.TrimSpace(kiroText(assistant["content"]))
			if content == "" {
				content = kiroAssistantPlaceholder
			}
			assistant["content"] = content
		}
	}
	return turns
}

func kiroContinueTurn(modelID string) map[string]any {
	return map[string]any{"userInputMessage": map[string]any{
		"content": kiroEmptyUserPlaceholder,
		"modelId": modelID,
		"origin":  "AI_EDITOR",
	}}
}

// kiroMergeUser folds one user turn into the previous one of the same role.
func kiroMergeUser(target, source map[string]any) {
	kiroAppendText(target, kiroText(source["content"]))
	if images, ok := source["images"].([]any); ok && len(images) > 0 {
		existing, _ := target["images"].([]any)
		target["images"] = append(existing, images...)
	}
	if results := kiroToolResults(source); len(results) > 0 {
		ctx, _ := target["userInputMessageContext"].(map[string]any)
		if ctx == nil {
			ctx = map[string]any{}
			target["userInputMessageContext"] = ctx
		}
		existing, _ := ctx["toolResults"].([]any)
		ctx["toolResults"] = append(existing, results...)
	}
}

// kiroMergeAssistant folds one assistant turn into the previous one.
func kiroMergeAssistant(target, source map[string]any) {
	kiroAppendText(target, kiroText(source["content"]))
	if uses, ok := source["toolUses"].([]any); ok && len(uses) > 0 {
		existing, _ := target["toolUses"].([]any)
		target["toolUses"] = append(existing, uses...)
	}
}

// kiroReconcileToolPair keeps only the tool calls that have a matching, valid
// result in the immediately following user turn, and only the results that answer
// one of those calls. Everything else is flattened into text.
func kiroReconcileToolPair(assistant, user map[string]any, turnIndex int, nameMap, specNames map[string]string, usedIDs map[string]bool, repairs *kiroRepairs) {
	calls, _ := assistant["toolUses"].([]any)
	results := kiroNormalizedToolResults(kiroToolResults(user))

	if len(calls) == 0 {
		if len(results) > 0 {
			kiroFlattenResults(user, kiroAsAnyList(results))
			repairs.OrphanResults += len(results)
		}
		if ctx, ok := user["userInputMessageContext"].(map[string]any); ok {
			delete(ctx, "toolResults")
		}
		kiroCleanUserContext(user)
		return
	}

	// Queue per source id: a repeated id answers its calls in order.
	queues := map[string][]int{}
	type callRecord struct {
		mappedName string
		input      any
		result     map[string]any
		validInput bool
	}
	records := make([]callRecord, 0, len(calls))
	for callIndex, raw := range calls {
		call, _ := raw.(map[string]any)
		key := kiroRawID(call["toolUseId"])
		name, _ := call["name"].(string)
		mapped := name
		if mappedName, ok := nameMap[name]; ok {
			mapped = mappedName
		}
		input, validInput := kiroNormalizeToolInput(call["input"])
		records = append(records, callRecord{mappedName: mapped, input: input, validInput: validInput})
		queues[key] = append(queues[key], callIndex)
	}

	var orphanResults []map[string]any
	for _, result := range results {
		key := kiroRawID(result["toolUseId"])
		assigned := false
		for _, idx := range queues[key] {
			if records[idx].result == nil {
				records[idx].result = result
				assigned = true
				break
			}
		}
		if !assigned {
			orphanResults = append(orphanResults, result)
		}
	}

	keptCalls := make([]any, 0, len(records))
	keptResults := make([]any, 0, len(records))
	for callIndex, record := range records {
		hasSpec := kiroHasSpec(specNames, record.mappedName)
		validPair := record.result != nil && hasSpec && record.validInput
		if !validPair {
			kiroAppendText(assistant, kiroToolCallText(record.mappedName, kiroCallInput(calls, callIndex)))
			if record.result == nil {
				repairs.MissingResults++
			}
			if !hasSpec || !record.validInput {
				repairs.InvalidToolUses++
			}
			if record.result != nil {
				kiroFlattenResults(user, []any{record.result})
				repairs.OrphanResults++
			}
			continue
		}
		id := kiroReserveToolID(kiroRawID(record.result["toolUseId"]), turnIndex, callIndex, record.mappedName, usedIDs)
		keptCalls = append(keptCalls, map[string]any{
			"toolUseId": id,
			"name":      record.mappedName,
			"input":     record.input,
		})
		copied := kiroCloneMap(record.result)
		copied["toolUseId"] = id
		keptResults = append(keptResults, copied)
	}

	if len(orphanResults) > 0 {
		kiroFlattenResults(user, kiroAsAnyList(orphanResults))
		repairs.OrphanResults += len(orphanResults)
	}
	if len(keptCalls) > 0 {
		assistant["toolUses"] = keptCalls
	} else {
		delete(assistant, "toolUses")
	}
	ctx, _ := user["userInputMessageContext"].(map[string]any)
	if len(keptResults) > 0 {
		if ctx == nil {
			ctx = map[string]any{}
			user["userInputMessageContext"] = ctx
		}
		ctx["toolResults"] = keptResults
	} else if ctx != nil {
		delete(ctx, "toolResults")
	}
	kiroCleanUserContext(user)
}

// kiroNormalizedToolResults converts raw toolResults into the gateway's shape:
// a non-empty status plus content that is never a single empty text part.
func kiroNormalizedToolResults(raw []any) []map[string]any {
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		result, ok := item.(map[string]any)
		if !ok {
			continue
		}
		parts := make([]any, 0, 2)
		switch content := result["content"].(type) {
		case []any:
			for _, part := range content {
				if text := kiroText(kiroPartText(part)); strings.TrimSpace(text) != "" {
					parts = append(parts, map[string]any{"text": text})
				}
			}
		default:
			text := kiroText(result["content"])
			if strings.TrimSpace(text) != "" {
				parts = append(parts, map[string]any{"text": text})
			}
		}
		if len(parts) == 0 {
			// An empty result is still a fact: CodeWhisperer rejects
			// [{text: ""}] with REQUEST_BODY_INVALID, so it carries a placeholder.
			parts = append(parts, map[string]any{"text": "(no output)"})
		}
		status := "success"
		if s, _ := result["status"].(string); s == "error" {
			status = "error"
		}
		out = append(out, map[string]any{
			"toolUseId": kiroRawID(result["toolUseId"]),
			"status":    status,
			"content":   parts,
		})
	}
	return out
}

// kiroFlattenResults appends each result to the user turn's text, so a pair the
// gateway cannot accept still reaches the model as context.
func kiroFlattenResults(user map[string]any, results []any) {
	for _, item := range results {
		result, ok := item.(map[string]any)
		if !ok {
			kiroAppendText(user, kiroText(item))
			continue
		}
		kiroAppendText(user, kiroToolResultText(result))
	}
}

// kiroCleanUserContext drops empty context containers so the gateway sees no
// hollow `userInputMessageContext`.
func kiroCleanUserContext(user map[string]any) {
	ctx, ok := user["userInputMessageContext"].(map[string]any)
	if !ok {
		return
	}
	if results, ok := ctx["toolResults"].([]any); !ok || len(results) == 0 {
		delete(ctx, "toolResults")
	}
	if tools, ok := ctx["tools"].([]any); !ok || len(tools) == 0 {
		delete(ctx, "tools")
	}
	if len(ctx) == 0 {
		delete(user, "userInputMessageContext")
	}
}

// kiroFlattenAllStructuredTools is the last resort: every tool block becomes
// text, so the conversation validates structurally even when nothing pairs.
func kiroFlattenAllStructuredTools(turns []map[string]any, repairs *kiroRepairs) {
	for _, turn := range turns {
		if assistant, ok := turn["assistantResponseMessage"].(map[string]any); ok {
			if uses, ok := assistant["toolUses"].([]any); ok && len(uses) > 0 {
				for _, use := range uses {
					call, _ := use.(map[string]any)
					name, _ := call["name"].(string)
					kiroAppendText(assistant, kiroToolCallText(name, call["input"]))
				}
				repairs.InvalidToolUses += len(uses)
				delete(assistant, "toolUses")
			}
		}
		user, ok := turn["userInputMessage"].(map[string]any)
		if !ok {
			continue
		}
		if results := kiroToolResults(user); len(results) > 0 {
			normalized := kiroNormalizedToolResults(results)
			flattened := make([]any, len(normalized))
			for i, result := range normalized {
				flattened[i] = result
			}
			kiroFlattenResults(user, flattened)
			repairs.OrphanResults += len(normalized)
			if ctx, ok := user["userInputMessageContext"].(map[string]any); ok {
				delete(ctx, "toolResults")
			}
			kiroCleanUserContext(user)
		}
	}
}

// kiroValidateConversation checks the strict wire shape without mutating it.
func kiroValidateConversation(history []map[string]any, current map[string]any, toolSpecs []any) kiroVerdict {
	turns := make([]map[string]any, 0, len(history)+1)
	turns = append(turns, history...)
	if current != nil {
		turns = append(turns, map[string]any{"userInputMessage": current})
	}
	specNames := kiroSpecNames(toolSpecs)
	usedIDs := map[string]bool{}
	var errs []string

	for i, turn := range turns {
		user, isUser := turn["userInputMessage"].(map[string]any)
		expectedUser := i%2 == 0
		if isUser != expectedUser {
			errs = append(errs, "role:"+strconv.Itoa(i))
		}
		if isUser {
			if i == 0 && len(kiroToolResults(user)) > 0 {
				errs = append(errs, "orphan:0")
			}
			continue
		}
		assistant, _ := turn["assistantResponseMessage"].(map[string]any)
		calls, _ := assistant["toolUses"].([]any)
		var results []any
		if next := i + 1; next < len(turns) {
			if nextUser, ok := turns[next]["userInputMessage"].(map[string]any); ok {
				results = kiroToolResults(nextUser)
			}
		}
		callIDs := make([]string, 0, len(calls))
		for _, raw := range calls {
			call, _ := raw.(map[string]any)
			callIDs = append(callIDs, kiroRawID(call["toolUseId"]))
		}
		resultIDs := make([]string, 0, len(results))
		for _, raw := range results {
			result, _ := raw.(map[string]any)
			resultIDs = append(resultIDs, kiroRawID(result["toolUseId"]))
		}
		if len(callIDs) != len(resultIDs) {
			errs = append(errs, "pair:"+strconv.Itoa(i))
		} else {
			sort.Strings(callIDs)
			sort.Strings(resultIDs)
			if strings.Join(callIDs, "\x00") != strings.Join(resultIDs, "\x00") {
				errs = append(errs, "pair:"+strconv.Itoa(i))
			}
		}
		for _, raw := range calls {
			call, _ := raw.(map[string]any)
			id := kiroRawID(call["toolUseId"])
			if id == "" || usedIDs[id] {
				errs = append(errs, "id:"+strconv.Itoa(i))
			}
			usedIDs[id] = true
			if name, _ := call["name"].(string); !kiroHasSpec(specNames, name) {
				errs = append(errs, "spec:"+strconv.Itoa(i))
			}
		}
	}
	if len(kiroText(current["content"])) == 0 {
		errs = append(errs, "current")
	}
	return kiroVerdict{valid: len(errs) == 0, errors: errs}
}

// kiroReserveToolID keeps a client's id when it is wire-safe and unique, and
// otherwise generates one, never exceeding the gateway's 64-character limit.
func kiroReserveToolID(value string, turnIndex, callIndex int, name string, usedIDs map[string]bool) string {
	sanitized := kiroSanitizeToolID(value)
	generated := "call_msg" + strconv.Itoa(turnIndex) + "_tc" + strconv.Itoa(callIndex) + "_" + kiroOr(name, "tool")
	base := sanitized
	if base == "" {
		base = generated
	}
	base = kiroTrimCodePoints(base, kiroToolIDMaxLength)
	candidate := base
	for suffix := 2; usedIDs[candidate]; suffix++ {
		tail := "_" + strconv.Itoa(suffix)
		candidate = kiroTrimCodePoints(base, kiroToolIDMaxLength-len(tail)) + tail
	}
	usedIDs[candidate] = true
	return candidate
}

// kiroNormalizeToolInput accepts an object, or a JSON object string, and reports
// false when the value cannot be a tool input at all.
func kiroNormalizeToolInput(input any) (any, bool) {
	switch value := input.(type) {
	case nil:
		return map[string]any{}, true
	case map[string]any:
		return kiroCloneMap(value), true
	case string:
		var parsed any
		if err := json.Unmarshal([]byte(value), &parsed); err != nil {
			return nil, false
		}
		if object, ok := parsed.(map[string]any); ok {
			return object, true
		}
		return nil, false
	default:
		return nil, false
	}
}

// kiroText renders any JSON value as the string Kiro sees.
func kiroText(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	default:
		encoded, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(encoded)
	}
}

// kiroAppendText extends a turn's content, separating blocks with a blank line.
func kiroAppendText(target map[string]any, extra string) {
	if extra == "" {
		return
	}
	if existing, _ := target["content"].(string); existing != "" {
		target["content"] = existing + "\n\n" + extra
		return
	}
	target["content"] = extra
}

// kiroToolCallText is how an unrepaired call still reaches the model.
func kiroToolCallText(name string, input any) string {
	return "[Tool call: " + kiroOr(name, "unknown") + "(" + kiroText(kiroOrCallInput(input)) + ")]"
}

// kiroToolResultText is how an unrepaired result still reaches the model.
func kiroToolResultText(result map[string]any) string {
	var b strings.Builder
	for _, part := range kiroContentTexts(result["content"]) {
		b.WriteString(part)
	}
	label := "[Tool result"
	if status, _ := result["status"].(string); status == "error" {
		label += " (error)"
	}
	return label + ": " + b.String() + "]"
}

// kiroContentTexts flattens a toolResult content value into plain strings.
func kiroContentTexts(content any) []string {
	switch value := content.(type) {
	case nil:
		return nil
	case string:
		return []string{value}
	case []any:
		out := make([]string, 0, len(value))
		for _, part := range value {
			if text := kiroText(kiroPartText(part)); text != "" {
				out = append(out, text)
			}
		}
		return out
	default:
		return []string{kiroText(value)}
	}
}

// kiroPartText reads a content part that may be a bare value or a {text: ...}.
func kiroPartText(part any) any {
	if object, ok := part.(map[string]any); ok {
		if text, exists := object["text"]; exists {
			return text
		}
	}
	return part
}

// kiroToolResults reads a turn's toolResults without asserting their type.
func kiroToolResults(user map[string]any) []any {
	ctx, _ := user["userInputMessageContext"].(map[string]any)
	if ctx == nil {
		return nil
	}
	results, _ := ctx["toolResults"].([]any)
	return results
}

// kiroSpecNames collects the tool names the catalog declared.
func kiroSpecNames(toolSpecs []any) map[string]string {
	names := make(map[string]string, len(toolSpecs))
	for _, raw := range toolSpecs {
		spec, _ := raw.(map[string]any)
		inner, _ := spec["toolSpecification"].(map[string]any)
		if name, _ := inner["name"].(string); name != "" {
			names[name] = name
		}
	}
	return names
}

// kiroAsAnyList re-types a typed slice for the flatten helpers, which accept any
// JSON value so a malformed history cannot fail the conversion.
func kiroAsAnyList(items []map[string]any) []any {
	out := make([]any, len(items))
	for i, item := range items {
		out[i] = item
	}
	return out
}

// kiroHasSpec reports whether the catalog declared this tool name.
func kiroHasSpec(specNames map[string]string, name string) bool {
	_, ok := specNames[name]
	return ok
}

func kiroRawID(value any) string {
	s, _ := value.(string)
	return s
}

func kiroSanitizeToolID(value string) string {
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		}
	}
	return b.String()
}

func kiroOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func kiroOrCallInput(input any) any {
	if input == nil {
		return map[string]any{}
	}
	return input
}

// kiroCallInput reads the raw input of the callIndex-th toolUse, for the flatten
// path that keeps the original payload in the text.
func kiroCallInput(calls []any, callIndex int) any {
	if callIndex < 0 || callIndex >= len(calls) {
		return nil
	}
	call, _ := calls[callIndex].(map[string]any)
	return call["input"]
}

// kiroClone deep-copies a JSON value so a repair cannot mutate the caller's body.
func kiroClone(value any) any {
	switch v := value.(type) {
	case map[string]any:
		return kiroCloneMap(v)
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = kiroClone(item)
		}
		return out
	default:
		return value
	}
}

func kiroCloneMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = kiroClone(v)
	}
	return out
}
