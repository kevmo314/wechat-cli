package main

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/kevmo314/wechat-cli/internal/native"
)

type chatLinkSpan struct {
	Start, End int
	URL        string
}
type chatLineRef struct {
	MessageID, QuoteTargetID, RichURL string
	Links                             []chatLinkSpan
}

func (m *chatModel) refreshHistory(bottom bool) {
	// Keep scrollback attached to its message when sync inserts earlier messages
	// or changes wrapping. A numeric line offset alone points at different text.
	anchorID, anchorLine := "", 0
	if !bottom {
		for i := m.viewport.YOffset; i < len(m.historyHits); i++ {
			id := m.historyHits[i].MessageID
			if start, ok := m.messageStarts[id]; id != "" && ok {
				anchorID, anchorLine = id, m.viewport.YOffset-start
				break
			}
		}
	}
	var lines []string
	m.historyHits = nil
	m.messageStarts = map[string]int{}
	add := func(block string, ref chatLineRef, links []string) {
		for _, line := range strings.Split(block, "\n") {
			row := ref
			row.Links = nil
			plain := ansi.Strip(line)
			for _, uri := range links {
				if i := strings.Index(plain, uri); i >= 0 {
					start := lipgloss.Width(plain[:i])
					row.Links = append(row.Links, chatLinkSpan{Start: start, End: start + lipgloss.Width(uri), URL: uri})
				}
			}
			lines = append(lines, line)
			m.historyHits = append(m.historyHits, row)
		}
	}
	lastDate := ""
	for _, v := range m.messages {
		if v.Peer != m.selected || !visibleChatMessage(v) {
			continue
		}
		date := time.Unix(int64(v.Timestamp), 0).Format("Monday, Jan 2, 2006")
		if date != lastDate {
			add(chatDim.Render("── "+date+" ──"), chatLineRef{}, nil)
			lastDate = date
		}
		m.messageStarts[v.ID] = len(lines)
		sender := m.name(v.From)
		if v.Type == 10000 || v.Type == 10002 {
			sender = "System"
		} else if v.Outgoing {
			sender = "You"
		} else if v.Sender != "" {
			sender = m.senderName(v.Peer, v.Sender)
		}
		label := ansi.Truncate(sender, max(8, m.viewport.Width/3), "…") + " " + time.Unix(int64(v.Timestamp), 0).Format("15:04") + "  "
		style := chatDim
		if v.Outgoing {
			style = chatTitle
		}
		if m.manualMessageSelection && m.selectedMessageID == v.ID {
			style = chatTitle.Bold(true)
		}
		text := messagePreview(v)
		if v.Deleted {
			text = chatDim.Render(text)
		}
		links := messageLinks(v)
		ref := chatLineRef{MessageID: v.ID}
		if v.Type == 49 && len(links) > 0 {
			ref.RichURL = links[0]
		}
		add(hangingMessage(style.Render(label)+text, m.viewport.Width), ref, links)
		if v.Type == 49 {
			for _, uri := range links {
				if strings.Contains(ansi.Strip(text), uri) {
					continue
				}
				label := compactLinkLabel(uri, max(1, m.viewport.Width-6))
				block := "  │ " + terminalLink(uri, label)
				add(chatDim.Render(block), chatLineRef{MessageID: v.ID, RichURL: uri}, nil)
			}
		}
		if !v.Deleted {
			if quoted, name, ok := m.referencedMessage(v); ok {
				if name == "" {
					name = m.replyName(quoted)
				}
				quote := name + ": " + oneLine(messagePreview(quoted))
				if m.expandedQuotes[v.ID] {
					quote = name + ": " + chatSafeText(messagePreview(quoted))
				} else {
					quote = ansi.Truncate(quote, max(1, m.viewport.Width-5), "…")
				}
				block := hangingMessage("  │ "+quote, m.viewport.Width)
				add(chatDim.Render(block), chatLineRef{MessageID: v.ID, QuoteTargetID: quoted.ID}, nil)
			}
		}
		if translated := m.translations[m.translationKey(v)]; translated != "" && !v.Deleted {
			add(chatDim.Render(hangingMessage("  Translation: "+chatSafeText(translated), m.viewport.Width)), chatLineRef{MessageID: v.ID}, nil)
		}
	}
	if m.sending && m.pendingPeer == m.selected {
		add(hangingMessage(chatDim.Render("You · sending  ")+chatSafeText(m.pendingText), m.viewport.Width), chatLineRef{}, nil)
	}
	if len(lines) == 0 {
		add(chatDim.Render("No messages yet.\n\nType below to start a conversation."), chatLineRef{}, nil)
	}
	m.viewport.SetContent(strings.Join(lines, "\n"))
	if bottom {
		m.viewport.GotoBottom()
		if m.chatVisible() {
			m.unread[m.selected] = 0
		}
	} else if start, ok := m.messageStarts[anchorID]; ok {
		end := start + 1
		for end < len(m.historyHits) && m.historyHits[end].MessageID == anchorID {
			end++
		}
		m.viewport.SetYOffset(min(start+anchorLine, end-1))
	}
}
func (m chatModel) selectedMessage() (native.Message, bool) {
	if m.manualMessageSelection {
		for _, v := range m.messages {
			if v.Peer == m.selected && v.ID == m.selectedMessageID {
				return v, true
			}
		}
	}
	for i := len(m.messages) - 1; i >= 0; i-- {
		v := m.messages[i]
		if v.Peer == m.selected && visibleChatMessage(v) && !v.Outgoing && !v.Deleted && v.Type != 10000 && v.Type != 10002 {
			return v, true
		}
	}
	for i := len(m.messages) - 1; i >= 0; i-- {
		v := m.messages[i]
		if v.Peer == m.selected && visibleChatMessage(v) && !v.Deleted {
			return v, true
		}
	}
	return native.Message{}, false
}
func (m *chatModel) beginReply(message native.Message) tea.Cmd {
	if message.Deleted {
		m.notice = "Deleted messages cannot be quoted"
		return nil
	}
	if message.Type == 10000 || message.Type == 10002 {
		m.notice = "System notices cannot be quoted"
		return nil
	}
	m.replyTargets[message.Peer] = message
	m.notice = "Replying to " + m.replyName(message) + " · Ctrl+X to cancel"
	return m.setFocus(focusCompose)
}
func (m chatModel) mouseAction(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if msg.Action != tea.MouseActionPress {
		return m, nil
	}
	leftW := min(34, max(24, m.width/3))
	if msg.Button == tea.MouseButtonLeft && msg.X < leftW && msg.Y >= 5 {
		index := m.listOffset + (msg.Y-5)/3
		if index >= 0 && index < len(m.rows) && msg.Y < m.height-3 {
			m.selectPeer(m.rows[index].Peer)
			m.selectedIndex = index
			m.ensureListVisible()
			return m, tea.Batch(m.setFocus(focusCompose), m.attention(false))
		}
		return m, nil
	}
	if msg.X < leftW+2 || msg.X >= m.width-2 || msg.Y < 4 || msg.Y >= 4+m.viewport.Height {
		return m, nil
	}
	row := msg.Y - 4 + m.viewport.YOffset
	if row < 0 || row >= len(m.historyHits) {
		return m, nil
	}
	hit := m.historyHits[row]
	if hit.MessageID == "" {
		return m, nil
	}
	m.selectedMessageID = hit.MessageID
	m.manualMessageSelection = true
	var message native.Message
	for _, v := range m.messages {
		if v.ID == hit.MessageID && v.Peer == m.selected {
			message = v
			break
		}
	}
	if msg.Button == tea.MouseButtonRight {
		return m, m.beginReply(message)
	}
	if msg.Button != tea.MouseButtonLeft {
		return m, nil
	}
	if hit.QuoteTargetID != "" {
		if start, ok := m.messageStarts[hit.QuoteTargetID]; ok {
			m.selectedMessageID = hit.QuoteTargetID
			m.refreshHistory(false)
			m.viewport.SetYOffset(max(0, start-1))
			m.notice = "Original quoted message"
		} else {
			m.expandedQuotes[hit.MessageID] = !m.expandedQuotes[hit.MessageID]
			m.refreshHistory(false)
			m.notice = "Quoted content expanded in this conversation"
		}
		return m, nil
	}
	uri := hit.RichURL
	x := msg.X - leftW - 2
	if uri == "" {
		for _, link := range hit.Links {
			if x >= link.Start && x < link.End {
				uri = link.URL
				break
			}
		}
	}
	if uri != "" {
		return m, func() tea.Msg { return openWebURL(uri) }
	}
	m.refreshHistory(false)
	m.notice = fmt.Sprintf("Selected %s's message · Ctrl+R reply · Ctrl+T translate", m.replyName(message))
	return m, nil
}
