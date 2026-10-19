package util

import "sync"

var claudeMetadataMu sync.Mutex

// ClaudeMetadataMutex returns the lock shared by Claude credential metadata accessors.
func ClaudeMetadataMutex() *sync.Mutex {
	return &claudeMetadataMu
}

// ReadClaudeMetadataValue reads a value and its presence under the credential metadata lock.
func ReadClaudeMetadataValue(metadata *map[string]any, key string) (any, bool) {
	if metadata == nil {
		return nil, false
	}
	claudeMetadataMu.Lock()
	defer claudeMetadataMu.Unlock()
	value, exists := (*metadata)[key]
	return value, exists
}
