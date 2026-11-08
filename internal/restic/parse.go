package restic

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
)

func decodeJSON[T any](data []byte, target *T) error {
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("decode restic JSON: %w", err)
	}
	return nil
}

func backupSummary(data []byte) (Summary, error) {
	var summary Summary
	found := false
	err := scanJSONLines(data, func(header jsonHeader, raw []byte) error {
		if header.MessageType != "summary" {
			return nil
		}
		found = true
		return json.Unmarshal(raw, &summary)
	})
	if err != nil {
		return Summary{}, err
	}
	if !found {
		return Summary{}, fmt.Errorf("restic backup returned no summary")
	}
	return summary, nil
}

func restoreSummary(data []byte) (RestoreSummary, error) {
	var summary RestoreSummary
	found := false
	err := scanJSONLines(data, func(header jsonHeader, raw []byte) error {
		if header.MessageType != "summary" {
			return nil
		}
		found = true
		return json.Unmarshal(raw, &summary)
	})
	if err != nil {
		return RestoreSummary{}, err
	}
	if !found {
		return RestoreSummary{}, fmt.Errorf("restic restore returned no summary")
	}
	return summary, nil
}

func checkSummary(data []byte) (CheckSummary, error) {
	var wire struct {
		MessageType string `json:"message_type"`
		CheckSummary
	}
	if err := decodeJSON(data, &wire); err != nil {
		return CheckSummary{}, err
	}
	if wire.MessageType != "summary" {
		return CheckSummary{}, fmt.Errorf("restic check returned no summary")
	}
	return wire.CheckSummary, nil
}

type jsonHeader struct {
	MessageType string `json:"message_type"`
	StructType  string `json:"struct_type"`
}

func scanJSONLines(data []byte, visit func(jsonHeader, []byte) error) error {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64<<10), 8<<20)
	for scanner.Scan() {
		raw := bytes.TrimSpace(scanner.Bytes())
		if len(raw) == 0 || raw[0] != '{' {
			continue
		}
		var header jsonHeader
		if err := json.Unmarshal(raw, &header); err != nil {
			continue
		}
		if err := visit(header, raw); err != nil {
			return fmt.Errorf("decode restic JSON line: %w", err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan restic JSON lines: %w", err)
	}
	return nil
}
