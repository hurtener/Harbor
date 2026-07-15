package app

import "strings"

// SelectItem is one reusable modal/select row.
type SelectItem struct {
	ID, Category, Title, Description string
	Current                          bool
	Actions                          []CommandID
}

// SelectModel stores search, category, current row, paging, context, backdrop,
// and focus restoration for every select-shaped dialog.
type SelectModel struct {
	Title, Query, Category  string
	Items                   []SelectItem
	Current, Page, PageSize int
	ContextActions          []CommandID
	CloseOnBackdrop         bool
	RestoreFocus            string
}

// NewSelect creates a copied, focus-restoring select model.
func NewSelect(title string, items []SelectItem, restoreFocus string) SelectModel {
	return SelectModel{Title: title, Items: cloneItems(items), PageSize: 8, CloseOnBackdrop: true, RestoreFocus: restoreFocus}
}

func cloneItems(items []SelectItem) []SelectItem {
	out := append([]SelectItem(nil), items...)
	for i := range out {
		out[i].Actions = append([]CommandID(nil), items[i].Actions...)
	}
	return out
}
func cloneSelect(m SelectModel) SelectModel {
	m.Items = cloneItems(m.Items)
	m.ContextActions = append([]CommandID(nil), m.ContextActions...)
	return m
}

// Visible returns search/category-filtered rows for the current page.
func (m SelectModel) Visible() []SelectItem {
	var filtered []SelectItem
	query := strings.ToLower(strings.TrimSpace(m.Query))
	for _, item := range m.Items {
		if m.Category != "" && item.Category != m.Category {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(item.Title+" "+item.Description), query) {
			continue
		}
		item.Actions = append([]CommandID(nil), item.Actions...)
		filtered = append(filtered, item)
	}
	start := min(max(0, m.Page)*max(1, m.PageSize), len(filtered))
	end := min(len(filtered), start+max(1, m.PageSize))
	return filtered[start:end]
}

// Move advances selection with wraparound.
func (m SelectModel) Move(delta int) SelectModel {
	rows := m.Visible()
	if len(rows) == 0 {
		m.Current = 0
		return m
	}
	m.Current = (m.Current + delta) % len(rows)
	if m.Current < 0 {
		m.Current += len(rows)
	}
	m.ContextActions = append([]CommandID(nil), rows[m.Current].Actions...)
	return m
}

// SetQuery resets paging and selection for a new filter.
func (m SelectModel) SetQuery(query string) SelectModel {
	m.Query = query
	m.Page = 0
	m.Current = 0
	return m.Move(0)
}

// SetCategory resets paging and selection for a category.
func (m SelectModel) SetCategory(category string) SelectModel {
	m.Category = category
	m.Page = 0
	m.Current = 0
	return m.Move(0)
}

// PageBy changes page without allowing a negative page.
func (m SelectModel) PageBy(delta int) SelectModel {
	m.Page = max(0, m.Page+delta)
	m.Current = 0
	return m.Move(0)
}

// BackdropClose reports whether a backdrop release should close this modal.
// Active text selection keeps it open so terminal-native selection is safe.
func (m SelectModel) BackdropClose(textSelectionActive bool) bool {
	return m.CloseOnBackdrop && !textSelectionActive
}

// FocusStack is one modal and focus restoration stack.
type FocusStack struct {
	base   string
	modals []SelectModel
}

// NewFocusStack starts with one base focus target.
func NewFocusStack(base string) FocusStack { return FocusStack{base: base} }

// Push adds a modal and captures the active focus target.
func (s FocusStack) Push(modal SelectModel) FocusStack {
	modal.RestoreFocus = s.Focus()
	s.modals = cloneModals(s.modals)
	s.modals = append(s.modals, cloneSelect(modal))
	return s
}

// Pop closes only the top modal and returns the restored focus.
func (s FocusStack) Pop() (FocusStack, string, bool) {
	if len(s.modals) == 0 {
		return s, s.base, false
	}
	top := s.modals[len(s.modals)-1]
	s.modals = cloneModals(s.modals[:len(s.modals)-1])
	return s, top.RestoreFocus, true
}

// Focus returns the top modal focus or base target.
func (s FocusStack) Focus() string {
	if len(s.modals) > 0 {
		return "modal"
	}
	return s.base
}

// Top returns the top select model.
func (s FocusStack) Top() (SelectModel, bool) {
	if len(s.modals) == 0 {
		return SelectModel{}, false
	}
	return cloneSelect(s.modals[len(s.modals)-1]), true
}

// ReplaceTop applies an updated select value.
func (s FocusStack) ReplaceTop(modal SelectModel) FocusStack {
	if len(s.modals) > 0 {
		s.modals = cloneModals(s.modals)
		s.modals[len(s.modals)-1] = cloneSelect(modal)
	}
	return s
}

func cloneModals(values []SelectModel) []SelectModel {
	out := make([]SelectModel, len(values))
	for i := range values {
		out[i] = cloneSelect(values[i])
	}
	return out
}
