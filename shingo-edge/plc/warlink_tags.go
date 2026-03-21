package plc

import "context"

// EnableTagPublishing tells WarLink to start publishing a tag.
func (m *Manager) EnableTagPublishing(ctx context.Context, plcName, tagName string) error {
	return m.wl.SetTagPublishing(ctx, plcName, tagName, true)
}

// DisableTagPublishing tells WarLink to stop publishing a tag.
func (m *Manager) DisableTagPublishing(ctx context.Context, plcName, tagName string) error {
	return m.wl.SetTagPublishing(ctx, plcName, tagName, false)
}

// FetchAllTags retrieves ALL tags (published and unpublished) from WarLink.
func (m *Manager) FetchAllTags(ctx context.Context, plcName string) ([]WarlinkTagInfo, error) {
	return m.wl.ListAllTags(ctx, plcName)
}

// IsTagPublished checks whether a tag is currently in the local WarLink cache
// (i.e. it's already being published and polled).
func (m *Manager) IsTagPublished(plcName, tagName string) bool {
	m.mu.RLock()
	mp, ok := m.plcs[plcName]
	m.mu.RUnlock()
	if !ok {
		return false
	}
	mp.mu.RLock()
	defer mp.mu.RUnlock()
	_, exists := mp.Values[tagName]
	return exists
}
