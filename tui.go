package main

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
	"github.com/kevmo314/wechat-cli/internal/native"
)

type chatBackend interface {
	sync(context.Context) chatSyncResult
	send(context.Context, string, string) chatSendResult
}
type chatTick struct{}
type chatConversation struct {
	Peer  string
	Last  native.Message
	Count int
}

const (
	focusCompose = iota
	focusSearch
)

var (
	chatGreen  = lipgloss.AdaptiveColor{Light: "#166534", Dark: "#7ee787"}
	chatMuted  = lipgloss.AdaptiveColor{Light: "#64748b", Dark: "#8b949e"}
	chatBorder = lipgloss.AdaptiveColor{Light: "#cbd5e1", Dark: "#303d49"}
	chatTitle  = lipgloss.NewStyle().Bold(true).Foreground(chatGreen)
	chatDim    = lipgloss.NewStyle().Foreground(chatMuted)
)

type chatModel struct {
	contactUI                         contactPage
	ctx                               context.Context
	backend                           chatBackend
	owner, profile                    string
	offline                           bool
	width, height                     int
	focus                             int
	search                            textinput.Model
	composer                          textarea.Model
	viewport                          viewport.Model
	messages                          []native.Message
	contacts                          map[string]native.Contact
	seen                              map[string]bool
	rows                              []chatConversation
	selected                          string
	selectedIndex, listOffset         int
	drafts                            map[string]string
	unread                            map[string]int
	query                             string
	newChat                           bool
	syncing, sending, paused, help    bool
	lastSync                          time.Time
	syncErr, notice                   string
	retries                           int
	pendingPeer, pendingText          string
	pendingAt                         uint32
	failedDrafts                      map[string]string
	activity                          map[string]uint64
	latest                            map[string]native.Message
	activityClock                     uint64
	historyHits                       []chatLineRef
	messageStarts                     map[string]int
	selectedMessageID                 string
	manualMessageSelection            bool
	expandedQuotes                    map[string]bool
	language                          string
	translations                      map[string]string
	translationBusy                   map[string]bool
	actionsOpen                       bool
	actionsItems                      []native.Message
	actionsIndex, actionsLinkIndex    int
	actionsDetail                     *native.Message
	actionsNotice                     string
	actionsDetailName                 string
	actionsScroll                     int
	replyTargets                      map[string]native.Message
	failedReplyTargets                map[string]native.Message
	mediaOpen, mediaLoading           bool
	mediaItems                        []native.Message
	mediaIndex                        int
	mediaPath, mediaPreview, mediaErr string
	readScheduled                     map[string]bool
	readAttempted                     map[string]bool
	rosterLoaded                      map[string]bool
	rosterLoading                     map[string]bool
	readBusy                          bool
	profileMenu                       bool
	profileChoice                     int
	nextProfile                       string
	terminalFocused                   bool
}

func newChatModel(ctx context.Context, backend chatBackend, owner, profile string, data chatSnapshot, offline bool) chatModel {
	profile, _ = native.NormalizeProfile(profile)
	search := textinput.New()
	search.Prompt = "/ "
	search.Placeholder = "Search chats"
	search.CharLimit = 256
	composer := textarea.New()
	composer.Prompt = ""
	composer.ShowLineNumbers = false
	composer.Placeholder = "Write a message…"
	composer.CharLimit = 60 * 1024
	composer.SetHeight(3)
	composer.MaxWidth = 0
	composer.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("alt+enter", "ctrl+j"))
	composer.KeyMap.Paste.SetEnabled(false) // Bracketed terminal paste works without a clipboard subprocess.
	m := chatModel{
		ctx: ctx, backend: backend, owner: owner, profile: profile, offline: offline, syncing: !offline,
		focus: focusCompose, search: search, composer: composer, viewport: viewport.New(60, 12),
		contacts: data.Contacts, seen: map[string]bool{}, drafts: map[string]string{}, unread: map[string]int{},
		rosterLoaded: map[string]bool{}, rosterLoading: map[string]bool{},
		readScheduled: map[string]bool{}, readAttempted: map[string]bool{},
		failedDrafts: map[string]string{}, expandedQuotes: map[string]bool{}, translations: map[string]string{}, translationBusy: map[string]bool{}, language: "en", replyTargets: map[string]native.Message{}, failedReplyTargets: map[string]native.Message{}, activity: map[string]uint64{}, latest: map[string]native.Message{},
		terminalFocused: true, width: 100, height: 30,
	}

	if m.contacts == nil {
		m.contacts = map[string]native.Contact{}
	}
	m.composer.Focus()
	m.addMessages(data.Messages, false)
	m.rebuildRows()
	m.resize(100, 30)
	return m
}
func (m chatModel) Init() tea.Cmd {
	if m.offline {
		return tea.Batch(m.composer.Focus(), m.attention(false))
	}
	return tea.Batch(m.composer.Focus(), m.attention(false), m.syncCommand())
}
func (m chatModel) syncCommand() tea.Cmd { return func() tea.Msg { return m.backend.sync(m.ctx) } }
func chatWait(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg { return chatTick{} })
}
func (m *chatModel) addMessages(messages []native.Message, incoming bool) {
	for _, v := range messages {
		native.NormalizeMessage(&v)
		if m.seen[v.ID] {
			for i := range m.messages {
				if m.messages[i].ID == v.ID {
					native.MergeMessageOrder(&m.messages[i], v)
					break
				}
			}
			if latest, ok := m.latest[v.Peer]; ok {
				if latest.ID == v.ID {
					native.MergeMessageOrder(&latest, v)
					m.latest[v.Peer] = latest
				} else if latest.Timestamp == v.Timestamp && v.Sequence != 0 && (latest.Sequence == 0 || v.Sequence > latest.Sequence) {
					m.latest[v.Peer] = v
				}
			}
			continue
		}
		m.seen[v.ID] = true
		m.messages = append(m.messages, v)
		if incoming && visibleChatMessage(v) {
			m.activityClock++
			m.activity[v.Peer] = m.activityClock
			previous, ok := m.latest[v.Peer]
			if !ok || v.Timestamp != previous.Timestamp || v.Sequence == 0 || previous.Sequence == 0 || v.Sequence >= previous.Sequence {
				m.latest[v.Peer] = v
			}
		}
		if incoming && !v.Outgoing && visibleChatMessage(v) && (v.Peer != m.selected || !m.chatVisible() || !m.viewport.AtBottom()) {
			m.unread[v.Peer]++
		}
	}
	native.ProjectMessages(m.messages)
	native.SortMessages(m.messages)
}
func visibleChatMessage(v native.Message) bool {
	if v.Hidden {
		return false
	}
	if v.Type == 10002 {
		_, ok := native.RecallFor(v)
		return ok
	}
	return v.Peer != "" && v.Type != 51 && v.Type != 9999
}
func chatSafeText(s string) string {
	s = ansi.Strip(strings.ToValidUTF8(s, "�"))
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, s)
}
func oneLine(s string) string { return strings.Join(strings.Fields(chatSafeText(s)), " ") }
func (m chatModel) name(peer string) string {
	if c, ok := m.contacts[peer]; ok {
		for _, name := range []string{c.Remark, c.Nickname, c.Alias} {
			if name = oneLine(name); name != "" {
				return name
			}
		}
	}
	if peer == m.owner {
		return "Saved messages"
	}
	if peer == "filehelper" {
		return "File Transfer"
	}
	return oneLine(peer)
}
func xmlText(s, element string) string {
	if len(s) > 128*1024 {
		return ""
	}
	decoder := xml.NewDecoder(strings.NewReader(s))
	for {
		token, err := decoder.Token()
		if err != nil {
			return ""
		}
		if start, ok := token.(xml.StartElement); ok && start.Name.Local == element {
			var text string
			if decoder.DecodeElement(&text, &start) == nil {
				return chatSafeText(text)
			}
			return ""
		}
	}
}
func messagePreview(v native.Message) string {
	if v.Deleted {
		if v.Type == 49 {
			if title := xmlText(v.Text, "title"); title != "" {
				return "[Deleted] " + title
			}
		}
		return "[Deleted message]"
	}
	switch v.Type {
	case 1:
		return linkify(chatSafeText(v.Text))
	case 3:
		return "▧ Image"
	case 34:
		return "◉ Voice message"
	case 43, 62:
		return "▷ Video"
	case 47:
		return "☺ Sticker"
	case 48:
		return "⌖ Location"
	case 49:
		kind := "Shared item"
		switch strings.TrimSpace(xmlText(v.Text, "type")) {
		case "5":
			kind = "Link"
		case "6":
			kind = "File"
		case "19":
			kind = "Forwarded messages"
		case "33", "36":
			kind = "Mini program"
		case "57":
			kind = "Reply"
		case "2000":
			kind = "Transfer"
		}
		if title := xmlText(v.Text, "title"); title != "" {
			label := "[" + kind + "] " + title
			if links := messageLinks(v); len(links) > 0 {
				return terminalLink(links[0], label)
			}
			return label
		}
		return "[" + kind + "]"
	case 37:
		return "Contact request"
	case 10002:
		if text := xmlText(v.Text, "replacemsg"); text != "" {
			return text
		}
		return "Conversation updated"
	case 10000:
		return chatSafeText(v.Text)
	default:
		return "Unsupported message · available in the CLI archive"
	}
}
func (m *chatModel) rebuildRows() {
	peers := map[string]chatConversation{}
	for _, v := range m.messages {
		if !visibleChatMessage(v) {
			continue
		}
		c := peers[v.Peer]
		c.Peer = v.Peer
		c.Last = v
		c.Count++
		peers[v.Peer] = c
	}
	for peer, contact := range m.contacts {
		if !m.newChat && !contact.Pinned() {
			continue
		}
		if _, ok := peers[peer]; !ok {
			peers[peer] = chatConversation{Peer: peer}
		}
	}
	if m.selected != "" {
		if _, ok := peers[m.selected]; !ok {
			peers[m.selected] = chatConversation{Peer: m.selected}
		}
	}
	m.rows = nil
	query := strings.ToLower(strings.TrimSpace(m.query))
	for peer, c := range peers {
		if latest, ok := m.latest[peer]; ok {
			c.Last = latest
		}
		if query != "" && !strings.Contains(strings.ToLower(m.name(c.Peer)+" "+c.Peer+" "+m.contacts[c.Peer].Alias), query) {
			continue
		}
		m.rows = append(m.rows, c)
	}
	sort.Slice(m.rows, func(i, j int) bool {
		if m.contacts[m.rows[i].Peer].Pinned() != m.contacts[m.rows[j].Peer].Pinned() {
			return m.contacts[m.rows[i].Peer].Pinned()
		}
		if m.activity[m.rows[i].Peer] != m.activity[m.rows[j].Peer] {
			return m.activity[m.rows[i].Peer] > m.activity[m.rows[j].Peer]
		}
		if m.rows[i].Last.Timestamp != m.rows[j].Last.Timestamp {
			return m.rows[i].Last.Timestamp > m.rows[j].Last.Timestamp
		}
		if a, b := m.name(m.rows[i].Peer), m.name(m.rows[j].Peer); a != b {
			return a < b
		}
		return m.rows[i].Peer < m.rows[j].Peer
	})
	if len(m.rows) == 0 && m.newChat && strings.TrimSpace(m.query) != "" && !strings.ContainsAny(m.query, " \n\r\t") {
		m.rows = []chatConversation{{Peer: strings.TrimSpace(m.query)}}
	}
	m.selectedIndex = -1
	for i, c := range m.rows {
		if c.Peer == m.selected {
			m.selectedIndex = i
			break
		}
	}
	if m.selectedIndex < 0 && len(m.rows) > 0 {
		m.selectPeer(m.rows[0].Peer)
		m.selectedIndex = 0
	}
	m.ensureListVisible()
}
func (m *chatModel) selectPeer(peer string) {
	if peer == m.selected {
		return
	}
	if m.selected != "" {
		m.drafts[m.selected] = m.composer.Value()
	}
	m.selected = peer
	m.selectedMessageID = ""
	m.manualMessageSelection = false
	m.composer.SetValue(m.drafts[peer])
	if m.chatVisible() {
		m.unread[peer] = 0
	}
	m.refreshHistory(true)
}
func (m *chatModel) ensureListVisible() {
	slots := max(1, (m.height-8)/3)
	if m.selectedIndex < m.listOffset {
		m.listOffset = max(0, m.selectedIndex)
	}
	if m.selectedIndex >= m.listOffset+slots {
		m.listOffset = m.selectedIndex - slots + 1
	}
	m.listOffset = min(m.listOffset, max(0, len(m.rows)-slots))
}
func (m *chatModel) setFocus(focus int) tea.Cmd {
	m.focus = focus
	m.search.Blur()
	m.composer.Blur()
	switch focus {
	case focusSearch:
		return m.search.Focus()
	case focusCompose:
		if m.viewport.AtBottom() && m.chatVisible() {
			m.unread[m.selected] = 0
		}
		return m.composer.Focus()
	}
	return nil
}
func (m *chatModel) resize(w, h int) {
	bottom := m.viewport.AtBottom()
	m.width = w
	m.height = h
	left := min(34, max(24, w/3))
	inner := max(16, w-left-4)
	m.search.Width = max(10, left-8)
	m.composer.SetWidth(inner)
	m.composer.SetHeight(3)
	m.viewport.Width = inner
	m.viewport.Height = max(1, h-12)
	m.ensureListVisible()
	m.refreshHistory(bottom)
}
func (m *chatModel) submit() tea.Cmd {
	if m.sending || m.selected == "" {
		return nil
	}
	if m.offline {
		m.notice = "Offline mode · sending is disabled"
		return nil
	}
	peer, text := m.selected, m.composer.Value()
	if strings.TrimSpace(text) == "" {
		return nil
	}
	if len(text) > 60*1024 {
		m.notice = "Message exceeds 60 KiB"
		return nil
	}
	m.sending = true
	var reply *native.Message
	if target, ok := m.replyTargets[peer]; ok {
		for _, current := range m.messages {
			if current.ID == target.ID && current.Peer == peer {
				target = current
				break
			}
		}
		if target.Deleted {
			m.notice = "Reply target was deleted · Ctrl+X to cancel"
			m.sending = false
			return nil
		}
		if _, ok := m.backend.(replyBackend); !ok {
			m.notice = "Quoted replies are unavailable"
			m.sending = false
			return nil
		}
		reply = &target
		delete(m.replyTargets, peer)
	}
	m.notice = "Sending…"
	m.pendingPeer, m.pendingText, m.pendingAt = peer, text, uint32(time.Now().Unix())
	delete(m.drafts, peer)
	m.composer.Reset()
	m.refreshHistory(true)
	if reply != nil {
		target := *reply
		name := publicQuoteName(m.contacts, target)
		return func() tea.Msg { return m.backend.(replyBackend).reply(m.ctx, target, text, name) }
	}
	return func() tea.Msg { return m.backend.send(m.ctx, peer, text) }
}
func (m chatModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	result, cmd := m.update(msg)
	next := result.(chatModel)
	if key, ok := msg.(tea.KeyMsg); ok && (key.String() == "ctrl+c" || key.String() == "ctrl+q") {
		return next, cmd
	}
	return next, tea.Batch(cmd, next.scheduleRead(), next.scheduleRoster())
}
func (m chatModel) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case chatContactResult:
		m.contactUI.Busy = false
		if msg.Accepted && msg.Contact.Username != "" {
			previous := m.contacts[msg.Peer]
			c := msg.Contact
			c.Flags = (previous.Flags &^ c.FlagMask) | (c.Flags & c.FlagMask)
			if len(c.Members) == 0 {
				c.Members = previous.Members
			}
			m.contacts[msg.Peer] = c
			m.rebuildRows()
			m.refreshHistory(m.viewport.AtBottom())
			m.contactUI.Editing = false
			m.contactUI.Editor.Blur()
		}
		if msg.Err != nil {
			m.contactUI.Notice = "Update failed: " + chatSafeText(msg.Err.Error())
		} else {
			m.contactUI.Notice = "Saved to WeChat"
		}
		m.notice = m.contactUI.Notice
		if m.contactUI.Open && !m.contactUI.Editing {
			return m, m.contactUI.Search.Focus()
		}
		return m, nil
	case chatTranslationResult:
		delete(m.translationBusy, msg.Key)
		if msg.Err != nil {
			m.notice = "Translation failed: " + chatSafeText(msg.Err.Error())
		} else {
			m.translations[msg.Key] = msg.Text
			m.refreshHistory(m.viewport.AtBottom())
			m.notice = "Translated by WeChat · Ctrl+T to hide"
		}
		return m, nil
	case chatLinkResult:
		if msg.Err != nil {
			m.notice = chatSafeText(msg.Err.Error())
		} else {
			m.notice = msg.Notice
		}
		return m, nil
	case chatRoomResult:
		delete(m.rosterLoading, msg.Peer)
		m.rosterLoaded[msg.Peer] = true
		if msg.Err == nil {
			c := m.contacts[msg.Peer]
			c.Members = msg.Members
			m.contacts[msg.Peer] = c
			m.refreshHistory(m.viewport.AtBottom())
		}
		return m, nil
	case chatReadDue:
		delete(m.readScheduled, msg.Peer+"/"+msg.MessageID)
		boundary, ok := m.readBoundary()
		if !ok || boundary.Peer != msg.Peer || boundary.ID != msg.MessageID || m.readBusy || m.readAttempted[msg.Peer+"/"+msg.MessageID] {
			return m, nil
		}
		backend, ok := m.backend.(readStateBackend)
		if !ok {
			return m, nil
		}
		m.readAttempted[msg.Peer+"/"+msg.MessageID] = true
		m.readBusy = true
		return m, func() tea.Msg { return backend.markRead(m.ctx, boundary) }
	case chatReadResult:
		m.readBusy = false
		if msg.Err != nil {
			m.notice = "Read-state sync failed: " + chatSafeText(msg.Err.Error())
		}
		return m, nil
	case chatAttachmentResult:
		m.mediaLoading = false
		m.mediaPath = msg.Path
		m.mediaPreview = msg.Preview
		if msg.Err != nil {
			m.mediaErr = chatSafeText(msg.Err.Error())
		} else {
			m.mediaErr = ""
		}
		return m, nil
	case tea.MouseMsg:
		if !m.contactUI.Open && !m.actionsOpen && !m.mediaOpen && !m.profileMenu && !m.help && (msg.Button == tea.MouseButtonLeft || msg.Button == tea.MouseButtonRight) {
			return m.mouseAction(msg)
		}
	case tea.FocusMsg:
		m.terminalFocused = true
		if m.viewport.AtBottom() && m.chatVisible() {
			m.unread[m.selected] = 0
		}
		return m, m.attention(false)
	case tea.BlurMsg:
		m.terminalFocused = false
		return m, nil
	case tea.WindowSizeMsg:
		m.resize(msg.Width, msg.Height)
		return m, nil
	case chatTick:
		if m.offline || m.paused || m.syncing {
			return m, nil
		}
		m.syncing = true
		return m, m.syncCommand()
	case chatSyncResult:
		m.syncing = false
		if msg.Err != nil {
			m.syncErr = chatSafeText(msg.Err.Error())
			m.retries++
			if msg.Retry && !m.offline {
				return m, chatWait(time.Duration(min(60, 3*(1<<min(m.retries, 4)))) * time.Second)
			}
			m.paused = true
			return m, nil
		}
		priorUnread := m.totalUnread()
		bottom := m.viewport.AtBottom()
		for peer, c := range msg.Contacts {
			previous := m.contacts[peer]
			if len(c.Members) == 0 {
				c.Members = previous.Members
			}
			c.Flags = (previous.Flags &^ c.FlagMask) | (c.Flags & c.FlagMask)
			m.contacts[peer] = c
		}
		m.addMessages(msg.Messages, true)
		m.rebuildRows()
		m.refreshHistory(bottom)
		m.lastSync = time.Now()
		m.syncErr = ""
		m.retries = 0
		if msg.More {
			m.syncing = true
			return m, tea.Batch(m.syncCommand(), m.attention(m.totalUnread() > priorUnread))
		}
		return m, tea.Batch(chatWait(3*time.Second), m.attention(m.totalUnread() > priorUnread))
	case chatSendResult:
		m.sending = false
		if msg.Accepted {
			m.addMessages([]native.Message{msg.Message}, true)

			m.rebuildRows()
			m.refreshHistory(true)
			m.notice = "Sent · accepted by WeChat"
		}
		if msg.Err != nil {
			if !msg.Accepted {
				if m.selected == msg.Peer && m.composer.Value() == "" {
					m.composer.SetValue(msg.Text)
					if msg.ReplyTo != nil {
						m.replyTargets[msg.Peer] = *msg.ReplyTo
					}
				} else if m.drafts[msg.Peer] == "" && m.selected != msg.Peer {
					m.drafts[msg.Peer] = msg.Text
					if msg.ReplyTo != nil {
						m.replyTargets[msg.Peer] = *msg.ReplyTo
					}
				} else {
					m.failedDrafts[msg.Peer] = msg.Text
					if msg.ReplyTo != nil {
						m.failedReplyTargets[msg.Peer] = *msg.ReplyTo
					}
				}
			}
			m.notice = chatSafeText(msg.Err.Error()) + " · check before retrying; Ctrl+Y restores failed text"
		}
		m.pendingPeer, m.pendingText = "", ""
		m.refreshHistory(true)
		return m, nil
	case tea.KeyMsg:
		if m.contactUI.Open {
			return m.contactKey(msg)
		}
		if !msg.Paste {
			if m.actionsOpen {
				return m.messageActionKey(msg.String())
			}
			if m.mediaOpen {
				return m.attachmentKey(msg.String())
			}
			if m.profileMenu {
				switch msg.String() {
				case "esc", "ctrl+p":
					m.profileMenu = false
					return m, nil
				case "up":
					m.profileChoice = (m.profileChoice + len(chatProfiles) - 1) % len(chatProfiles)
					return m, nil
				case "down":
					m.profileChoice = (m.profileChoice + 1) % len(chatProfiles)
					return m, nil
				case "enter":
					choice := chatProfiles[m.profileChoice].Name
					if choice == m.profile {
						m.profileMenu = false
						return m, nil
					}
					m.nextProfile = choice
					return m, tea.Quit
				case "ctrl+c", "ctrl+q":
					return m, tea.Quit
				}
				return m, nil
			}
			if m.help {
				if msg.String() == "c" {
					cmd := m.openContacts()
					return m, cmd
				}
				if msg.String() == "ctrl+c" || msg.String() == "ctrl+q" {
					return m, tea.Quit
				}
				m.help = false
				return m, nil
			}
			switch msg.String() {
			case "ctrl+c", "ctrl+q":
				return m, tea.Quit
			case "ctrl+e":
				m.openMessageActions()
				return m, nil
			case "ctrl+x":
				delete(m.replyTargets, m.selected)
				m.notice = "Reply cancelled"
				return m, nil
			case "ctrl+o":
				m.openAttachments()
				return m, nil
			case "ctrl+p":
				m.profileMenu = true
				m.profileChoice = profileIndex(m.profile)
				return m, nil
			case "alt+c":
				cmd := m.openContacts()
				return m, cmd
			case "ctrl+g":
				m.help = true
				return m, nil
			case "ctrl+k":
				m.newChat = false
				return m, m.setFocus(focusSearch)
			case "ctrl+n":
				m.newChat = true
				m.query = ""
				m.search.SetValue("")
				m.rebuildRows()
				return m, m.setFocus(focusSearch)
			case "ctrl+t":
				return m, m.translateSelected()
			case "ctrl+r":
				if message, ok := m.selectedMessage(); ok {
					return m, m.beginReply(message)
				}
				return m, nil
			case "alt+s":
				if !m.syncing && !m.offline {
					m.paused = false
					m.syncing = true
					return m, m.syncCommand()
				}
				return m, nil
			case "ctrl+s":
				return m, m.submit()
			case "ctrl+y":
				if text := m.failedDrafts[m.selected]; text != "" {
					m.failedDrafts[m.selected] = m.composer.Value()
					failed, hasFailed := m.failedReplyTargets[m.selected]
					current, hasCurrent := m.replyTargets[m.selected]
					delete(m.replyTargets, m.selected)
					delete(m.failedReplyTargets, m.selected)
					if hasFailed {
						m.replyTargets[m.selected] = failed
					}
					if hasCurrent {
						m.failedReplyTargets[m.selected] = current
					}
					m.composer.SetValue(text)
				}
				return m, nil
			case "pgup":
				m.viewport.HalfPageUp()
				return m, nil
			case "pgdown":
				m.viewport.HalfPageDown()
				if m.viewport.AtBottom() {
					m.unread[m.selected] = 0
				}
				return m, m.attention(false)
			case "ctrl+home":
				m.viewport.GotoTop()
				return m, nil
			case "ctrl+end":
				m.viewport.GotoBottom()
				m.unread[m.selected] = 0
				return m, m.attention(false)
			case "esc":
				m.query = ""
				m.search.SetValue("")
				m.newChat = false
				m.rebuildRows()
				return m, m.setFocus(focusCompose)
			case "tab", "shift+tab":
				return m, nil
			case "up", "down":
				if len(m.rows) > 0 {
					delta := 1
					if msg.String() == "up" {
						delta = -1
					}
					m.selectedIndex = max(0, min(len(m.rows)-1, m.selectedIndex+delta))
					m.selectPeer(m.rows[m.selectedIndex].Peer)
					m.ensureListVisible()
				}
				return m, m.attention(false)
			case "enter":
				if m.focus == focusSearch {
					if len(m.rows) == 0 {
						m.notice = "No matching contact"
						return m, nil
					}
					return m, m.setFocus(focusCompose)
				}
				return m, m.submit()
			}
		}
	}

	if m.contactUI.Open {
		return m, nil
	}
	var cmd tea.Cmd
	switch m.focus {
	case focusSearch:
		m.search, cmd = m.search.Update(msg)
		if m.query != m.search.Value() {
			m.query = m.search.Value()
			m.rebuildRows()
			m.refreshHistory(true)
		}
	case focusCompose:
		m.composer, cmd = m.composer.Update(msg)
	}
	if _, ok := msg.(tea.MouseMsg); ok {
		m.viewport, cmd = m.viewport.Update(msg)
	}
	return m, cmd
}
func chatPanel(content string, w, h int, focused bool) string {
	color := chatBorder
	if focused {
		color = chatGreen
	}
	lines := strings.Split(content, "\n")
	if len(lines) > max(1, h-2) {
		lines = lines[:max(1, h-2)]
	}
	for i, line := range lines {
		lines[i] = ansi.Truncate(line, max(1, w-4), "…")
	}
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(color).Padding(0, 1).Width(max(1, w-2)).Height(max(1, h-2)).Render(strings.Join(lines, "\n"))
}
func (m chatModel) View() string {
	if m.contactUI.Open {
		return m.contactsView()
	}
	if m.actionsOpen {
		return m.messageActionsView()
	}
	if m.mediaOpen {
		return m.attachmentsView()
	}
	if m.profileMenu {
		return profilePickerView(m.profileChoice, m.width, m.height)
	}
	if m.width < 64 || m.height < 18 {
		return "WeChat\nResize the terminal to at least 64 × 18.\nCtrl+C to quit."
	}
	if m.help {
		help := "WeChat · keyboard shortcuts\n\n↑ ↓                 Switch chats; composer stays focused\nPgUp / PgDn         Scroll conversation history\nEnter / Ctrl+S      Send message\nAlt+Enter           New line in the message\nCtrl+K / Ctrl+N     Search conversations / new chat\nMouse click          Select message / open its link\nRight-click / Ctrl+R Reply immediately\nClick quoted preview Jump to original quoted message\nCtrl+E              Optional message details\nCtrl+T              Translate selected message / hide translation\nCtrl+X              Cancel the selected reply target\nCtrl+O              Attachments and image previews\nCtrl+P              Choose phone / tablet / desktop\nAlt+C / c here     Contacts: remarks and pinned chats\nCtrl+G / Esc        Help / close current view\nCtrl+Y              Restore a failed send\nAlt+S               Refresh or retry synchronization\nCtrl+Home / End     Oldest / newest message\nCtrl+C / Ctrl+Q     Quit (drafts are saved)\n\nImages and files can be downloaded. Voice/video downloads and media uploads\nare not implemented. Read state syncs only while the active chat is visible."

		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, chatPanel(ansi.Wrap(help, m.width-10, ""), min(m.width-4, 78), min(m.height-2, 25), true))
	}
	state := "Connecting"
	if !m.lastSync.IsZero() {
		state = "Connected · " + m.lastSync.Format("15:04:05")
	}
	if m.syncing {
		state = "Syncing…"
	}
	if m.offline {
		state = "Offline archive"
	}
	if m.syncErr != "" {
		state = "Sync stopped · Alt+S retry"
	}
	profile := profileLabel(m.profile)
	brandText := "●  WeChat"
	if n := m.totalUnread(); n > 0 {
		brandText += fmt.Sprintf(" · %d unread", n)
	}
	brand := chatTitle.Render(brandText) + chatDim.Render("  "+profile)
	top := brand + strings.Repeat(" ", max(1, m.width-lipgloss.Width(brand)-lipgloss.Width(state)-2)) + chatDim.Render(state)
	leftW := min(34, max(24, m.width/3))
	bodyH := m.height - 3
	rightW := m.width - leftW
	leftInner := leftW - 4
	title := "CONVERSATIONS"
	if m.newChat {
		title = "CONTACTS"
	}
	sidebar := chatTitle.Render(title) + chatDim.Render(fmt.Sprintf("  %d", len(m.rows))) + "\n"
	if m.focus == focusSearch {
		sidebar += m.search.View()
	} else if m.query != "" {
		sidebar += chatDim.Render("/ " + ansi.Truncate(oneLine(m.query), leftInner-2, "…"))
	} else {
		sidebar += chatDim.Render("/ Search    n New chat")
	}
	sidebar += "\n\n"
	slots := max(1, (m.height-8)/3)
	for i := m.listOffset; i < min(len(m.rows), m.listOffset+slots); i++ {
		row := m.rows[i]
		name := m.name(row.Peer)
		if m.contacts[row.Peer].Pinned() {
			name = "PIN · " + name
		}
		if m.unread[row.Peer] > 0 {
			name += fmt.Sprintf(" · %d", m.unread[row.Peer])
		}
		preview := "Start a conversation"
		if row.Count > 0 {
			preview = oneLine(messagePreview(row.Last))
		}
		if draft := m.drafts[row.Peer]; draft != "" {
			preview = "Draft: " + oneLine(draft)
		}
		text := ansi.Truncate(name, leftInner, "…") + "\n" + ansi.Truncate(preview, leftInner, "…")
		if row.Peer == m.selected {
			text = lipgloss.NewStyle().Background(lipgloss.Color("#174b37")).Foreground(lipgloss.Color("#ffffff")).Width(leftInner).Render(text)
		} else {
			text = chatDim.Render(text)
		}
		if m.contacts[row.Peer].Pinned() && i+1 < len(m.rows) && !m.contacts[m.rows[i+1].Peer].Pinned() {
			sidebar += text + "\n" + chatDim.Render("── CONVERSATIONS ──") + "\n"
		} else {
			sidebar += text + "\n\n"
		}
	}
	if len(m.rows) == 0 {
		sidebar += chatDim.Render("No matching conversations.\nCtrl+N to find a contact.")
	}
	name := m.name(m.selected)
	if m.selected == "" {
		name = "Welcome to WeChat"
	}
	inner := rightW - 4
	header := ansi.Truncate(chatTitle.Render(name)+chatDim.Render("  "+oneLine(m.selected)), inner, "…") + "\n" + chatDim.Render(strings.Repeat("─", inner))
	label := "MESSAGE  ·  Enter send  ·  Alt+Enter newline"
	if target, ok := m.replyTargets[m.selected]; ok {
		label = "REPLY to " + m.replyName(target) + ": " + oneLine(messagePreview(target)) + " · Ctrl+X cancel"
	}
	if m.sending {
		label = "SENDING…"
	}
	if m.offline {
		label = "OFFLINE  ·  Drafts only"
	}
	thread := header + "\n" + m.viewport.View() + "\n" + chatDim.Render(strings.Repeat("─", inner)) + "\n" + chatDim.Render(ansi.Truncate(label, inner, "…")) + "\n" + m.composer.View()
	body := lipgloss.JoinHorizontal(lipgloss.Top, chatPanel(sidebar, leftW, bodyH, m.focus == focusSearch), chatPanel(thread, rightW, bodyH, m.focus == focusCompose))
	status := m.notice
	if status == "" {
		status = m.syncErr
	}
	if status == "" {
		status = "Drafts stay with each conversation.  Messages are sent only on Enter or Ctrl+S."
	}
	footer := " ↑↓ chats · PgUp/Dn scroll · Ctrl+R reply · Ctrl+T translate · Ctrl+G help · Ctrl+Q quit"
	return top + "\n" + body + "\n" + chatDim.Render(ansi.Truncate(oneLine(status), m.width, "…")) + "\n" + chatDim.Render(ansi.Truncate(footer, m.width, "…"))
}

func nativeTUI(ctx context.Context, store Store, args []string, requested string) error {
	f := flags("tui")
	offline := f.Bool("offline", false, "browse the local archive without connecting or sending")
	language := f.String("language", "en", "message translation target language")
	if err := parse(f, args); err != nil {
		return err
	}
	if !native.ValidTranslationLanguage(*language) {
		return errors.New("invalid translation language")
	}
	if !term.IsTerminal(os.Stdin.Fd()) || !term.IsTerminal(os.Stdout.Fd()) {
		return errors.New("the TUI requires a terminal; use wechat read --json or wechat history for pipelines")
	}
	s, err := openNativeStore(store)
	if err != nil {
		return err
	}
	state, err := s.state()
	if err != nil {
		return err
	}
	if state.Account == nil {
		if *offline {
			return errors.New("no saved account; run wechat login first")
		}
		if requested == "" {
			choice, err := chooseProfile(ctx)
			if err != nil {
				return err
			}
			return &profileSwitchRequest{Profile: choice}
		}
		if err = nativeLogin(ctx, store, s, []string{"--profile", requested}); err != nil {
			return err
		}
		state, err = s.state()
		if err != nil {
			return err
		}
	}
	if !*offline {
		reader, err := store.readerLock()
		if err != nil {
			return err
		}
		defer reader.Close()
	}
	data, err := s.chatSnapshot(state.Account.Username)
	if err != nil {
		return err
	}
	client := &chatService{store: s, owner: state.Account.Username}
	uiCtx, cancel := context.WithCancel(ctx)
	model := newChatModel(uiCtx, client, state.Account.Username, state.Account.Profile, data, *offline)
	model.language = *language
	var saved chatDraftState
	if loadErr := s.load("drafts.json", &saved); loadErr == nil && saved.Owner == model.owner {
		if saved.Drafts != nil {
			model.drafts = saved.Drafts
		}
		if saved.Replies != nil {
			model.replyTargets = saved.Replies
		}
		if saved.FailedReplies != nil {
			model.failedReplyTargets = saved.FailedReplies
		}
		if saved.Failed != nil {
			model.failedDrafts = saved.Failed
		}
		if saved.Unread != nil {
			model.unread = saved.Unread
		}
		model.composer.SetValue(model.drafts[model.selected])
		model.unread[model.selected] = 0
	}
	final, runErr := tea.NewProgram(model, tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithReportFocus(), tea.WithContext(uiCtx)).Run()
	cancel()
	client.close()
	err = runErr
	if result, ok := final.(chatModel); ok {
		if result.selected != "" {
			result.drafts[result.selected] = result.composer.Value()
		}
		if saveErr := s.save("drafts.json", chatDraftState{Owner: result.owner, Drafts: result.drafts, Failed: result.failedDrafts, Unread: result.unread, Replies: result.replyTargets, FailedReplies: result.failedReplyTargets}); saveErr != nil {
			return fmt.Errorf("could not save drafts: %w", saveErr)
		}
		if result.nextProfile != "" {
			return &profileSwitchRequest{Profile: result.nextProfile}
		}
	}
	if errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

func (m chatModel) totalUnread() int {
	n := 0
	for _, count := range m.unread {
		n += count
	}
	return n
}
func (m chatModel) attention(bell bool) tea.Cmd {
	title := "WeChat"
	if n := m.totalUnread(); n > 0 {
		title = fmt.Sprintf("WeChat — %d unread", n)
	}
	var notify tea.Cmd
	if bell {
		notify = func() tea.Msg { fmt.Fprint(os.Stderr, "\a"); return nil }
	}
	return tea.Batch(tea.SetWindowTitle(title), notify)
}

func hangingMessage(text string, width int) string {
	lines := strings.Split(ansi.Wrap(text, max(1, width-2), ""), "\n")
	for i := 1; i < len(lines); i++ {
		lines[i] = "  " + lines[i]
	}
	return strings.Join(lines, "\n")
}
func (m *chatModel) scheduleRead() tea.Cmd {
	if m.readBusy {
		return nil
	}
	boundary, ok := m.readBoundary()
	if !ok {
		return nil
	}
	key := boundary.Peer + "/" + boundary.ID
	if m.readAttempted[key] || m.readScheduled[key] {
		return nil
	}
	if _, ok := m.backend.(readStateBackend); !ok {
		return nil
	}
	m.readScheduled[key] = true
	return tea.Tick(800*time.Millisecond, func(time.Time) tea.Msg { return chatReadDue{Peer: boundary.Peer, MessageID: boundary.ID} })
}

func (m *chatModel) scheduleRoster() tea.Cmd {
	if m.offline || m.contactUI.Open || m.profileMenu || m.mediaOpen || m.help || !strings.HasSuffix(m.selected, "@chatroom") || m.rosterLoaded[m.selected] || m.rosterLoading[m.selected] {
		return nil
	}
	backend, ok := m.backend.(roomBackend)
	if !ok {
		return nil
	}
	peer := m.selected
	m.rosterLoading[peer] = true
	return func() tea.Msg { return backend.room(m.ctx, peer) }
}

func (m chatModel) chatVisible() bool {
	return m.terminalFocused && !m.contactUI.Open && !m.actionsOpen && !m.profileMenu && !m.mediaOpen && !m.help && m.focus != focusSearch
}
