package sqlite

import (
	"encoding/json"
	"errors"

	"gorm.io/gorm"
)

// Old agent transcripts used signature for the field Pi calls thinkingSignature.
func migrateThinkingSignatures(db *gorm.DB) error {
	for _, column := range []string{"agent_messages", "compacted_context"} {
		var after string
		for {
			var record struct {
				ID      string
				History []byte
			}
			err := db.Table("runs").Select("id, "+column+" AS history").
				Where("id > ? AND instr("+column+", ?) > 0", after, `"signature"`).
				Order("id ASC").Take(&record).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				break
			}
			if err != nil {
				return err
			}
			history, err := normalizeThinkingSignatures(record.History)
			if err != nil {
				return err
			}
			if history != nil {
				if err := db.Table("runs").Where("id = ?", record.ID).UpdateColumn(column, history).Error; err != nil {
					return err
				}
			}
			after = record.ID
		}
	}
	return nil
}

func normalizeThinkingSignatures(history []byte) ([]byte, error) {
	var messages []json.RawMessage
	if err := json.Unmarshal(history, &messages); err != nil {
		// Leave malformed historical data intact so it can be recovered later.
		return nil, nil
	}
	changed := false
	for i, message := range messages {
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(message, &envelope); err != nil || string(envelope["role"]) != `"assistant"` {
			continue
		}
		content, err := normalizeThinkingBlocks(envelope["content"])
		if err != nil {
			return nil, err
		}
		if content == nil {
			continue
		}
		envelope["content"] = content
		messages[i], err = json.Marshal(envelope)
		if err != nil {
			return nil, err
		}
		changed = true
	}
	if !changed {
		return nil, nil
	}
	return json.Marshal(messages)
}

func normalizeThinkingBlocks(data json.RawMessage) ([]byte, error) {
	var blocks []json.RawMessage
	if err := json.Unmarshal(data, &blocks); err != nil {
		return nil, nil
	}
	changed := false
	for i, data := range blocks {
		var block map[string]json.RawMessage
		if err := json.Unmarshal(data, &block); err != nil || string(block["type"]) != `"thinking"` {
			continue
		}
		var legacy, current string
		if err := json.Unmarshal(block["signature"], &legacy); err != nil || legacy == "" {
			continue
		}
		if value := block["thinkingSignature"]; value != nil {
			if err := json.Unmarshal(value, &current); err != nil || current != "" {
				continue
			}
		}
		block["thinkingSignature"] = block["signature"]
		delete(block, "signature")
		entry, err := json.Marshal(block)
		if err != nil {
			return nil, err
		}
		blocks[i] = entry
		changed = true
	}
	if !changed {
		return nil, nil
	}
	return json.Marshal(blocks)
}
