package main

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/kevmo314/wechat-cli/internal/native"
)

func refreshTestModel() chatModel {
	m := testChatModel()
	var messages []native.Message
	for i := 0; i < 80; i++ {
		peer := fmt.Sprintf("peer-%02d", i)
		messages = append(messages, native.Message{ID: peer, Peer: peer, From: peer, Type: 1, Text: "A chat", Timestamp: uint32(100 + i)})
		messages = append(messages, native.Message{ID: fmt.Sprintf("alice-%02d", i), Peer: "alice", From: "alice", Type: 1, Text: fmt.Sprintf("消息 %02d: preserved scroll position", i), Timestamp: uint32(100 + i)})
	}
	m.addMessages(messages, false)
	m.rebuildRows()
	m.resize(100, 30)
	return m
}

func TestTUIRefreshKeepsSelectedConversationVisible(t *testing.T) {
	m := refreshTestModel()
	m.selectPeer("peer-00")
	m.rebuildRows()
	m.composer.SetValue("Keep this draft")
	next, _ := m.update(chatSyncResult{Messages: []native.Message{{ID: "new-top", Peer: "peer-79", From: "peer-79", Type: 1, Text: "New arrival", Timestamp: 300}}})
	m = next.(chatModel)
	slots := max(1, (m.height-8)/3)
	if m.selectedIndex < m.listOffset || m.selectedIndex >= m.listOffset+slots {
		t.Fatalf("selected conversation scrolled out of view: index=%d offset=%d slots=%d", m.selectedIndex, m.listOffset, slots)
	}
	if m.selected != "peer-00" || m.composer.Value() != "Keep this draft" {
		t.Fatal("refresh changed the recipient or draft")
	}
}

func TestTUIRefreshPreservesScrolledMessage(t *testing.T) {
	m := refreshTestModel()
	m.viewport.SetYOffset(m.messageStarts["alice-40"])
	before := ansi.Strip(strings.Split(m.viewport.View(), "\n")[0])
	next, _ := m.update(chatSyncResult{Messages: []native.Message{{ID: "backfill", Peer: "alice", From: "alice", Type: 1, Text: "Earlier message", Timestamp: 50}}})
	m = next.(chatModel)
	if after := ansi.Strip(strings.Split(m.viewport.View(), "\n")[0]); after != before {
		t.Fatalf("refresh moved the visible message:\nbefore: %q\nafter:  %q", before, after)
	}
}

func TestTUIResizePreservesScrollback(t *testing.T) {
	m := refreshTestModel()
	m.viewport.SetYOffset(m.messageStarts["alice-40"])
	next, _ := m.update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = next.(chatModel)
	if ref := m.historyHits[m.viewport.YOffset]; ref.MessageID != "alice-40" {
		t.Fatalf("resize moved scrollback to %q", ref.MessageID)
	}
}

func TestTUIRefreshKeepsFollowingNewestMessage(t *testing.T) {
	m := refreshTestModel()
	m.viewport.GotoBottom()
	next, _ := m.update(chatSyncResult{Messages: []native.Message{{ID: "newest", Peer: "alice", From: "alice", Type: 1, Text: "Latest arrival", Timestamp: 500}}})
	m = next.(chatModel)
	if !m.viewport.AtBottom() || !strings.Contains(ansi.Strip(m.viewport.View()), "Latest arrival") {
		t.Fatal("refresh stopped following new messages at the bottom")
	}
	m.resize(80, 24)
	if !m.viewport.AtBottom() {
		t.Fatal("resize stopped following the bottom")
	}
}

func TestTUIResizeKeepsWrappedMessageVisible(t *testing.T) {
	m := refreshTestModel()
	for i := range m.messages {
		if m.messages[i].ID == "alice-40" {
			m.messages[i].Text = strings.Repeat("A long message with several wrapped lines. ", 20)
		}
	}
	m.resize(64, 24)
	m.viewport.SetYOffset(m.messageStarts["alice-40"] + 7)
	m.resize(190, 50)
	if ref := m.historyHits[m.viewport.YOffset]; ref.MessageID != "alice-40" {
		t.Fatalf("widening the window moved past the wrapped message to %q", ref.MessageID)
	}
}

func TestTUIRefreshOrdersIdenticalConversationLabels(t *testing.T) {
	m := testChatModel()
	for i := 0; i < 40; i++ {
		peer := fmt.Sprintf("same-%02d", i)
		m.contacts[peer] = native.Contact{Username: peer, Nickname: "Same name", Flags: 0x803}
	}
	var previous string
	for i := 0; i < 10; i++ {
		m.rebuildRows()
		var peers []string
		for _, row := range m.rows {
			peers = append(peers, row.Peer)
		}
		order := strings.Join(peers, ",")
		if i > 0 && order != previous {
			t.Fatal("refresh changed the order of identical conversation labels")
		}
		previous = order
	}
}

func TestTUIRefreshFrameFitsTerminal(t *testing.T) {
	for _, size := range [][2]int{{64, 18}, {80, 24}, {100, 30}, {190, 50}} {
		for _, text := range []string{"你好 👋 中文与 emoji 👨‍👩‍👧‍👦", "one\ttwo\tthree", strings.Repeat("https://example.com/very-long-path/", 10), strings.Repeat("a\n", 30)} {
			m := refreshTestModel()
			m.resize(size[0], size[1])
			m.composer.SetValue(text)
			next, _ := m.update(chatSyncResult{Messages: []native.Message{{ID: "rich", Peer: "alice", From: "alice", Type: 1, Text: text, Timestamp: 500}}})
			m = next.(chatModel)
			view := m.View()
			if height := lipgloss.Height(view); height != size[1] {
				t.Errorf("%v: height=%d", size, height)
			}
			for i, line := range strings.Split(view, "\n") {
				if width := lipgloss.Width(line); width > size[0] {
					t.Errorf("%v row %d: width=%d", size, i, width)
				}
			}
			if strings.Contains(view, "\t") {
				t.Errorf("%v: tab escaped layout", size)
			}
		}
	}
}
