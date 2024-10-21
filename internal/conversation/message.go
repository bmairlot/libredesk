package conversation

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/abhinavxd/artemis/internal/attachment"
	"github.com/abhinavxd/artemis/internal/conversation/models"
	"github.com/abhinavxd/artemis/internal/envelope"
	"github.com/abhinavxd/artemis/internal/inbox"
	mmodels "github.com/abhinavxd/artemis/internal/media/models"
	"github.com/abhinavxd/artemis/internal/stringutil"
	umodels "github.com/abhinavxd/artemis/internal/user/models"
	"github.com/lib/pq"
)

const (
	MessageIncoming = "incoming"
	MessageOutgoing = "outgoing"
	MessageActivity = "activity"

	SenderTypeUser    = "user"
	SenderTypeContact = "contact"

	MessageStatusPending   = "pending"
	MessageStatusSent      = "sent"
	MessageStatusDelivered = "delivered"
	MessageStatusRead      = "read"
	MessageStatusFailed    = "failed"
	MessageStatusReceived  = "received"

	ActivityStatusChange       = "status_change"
	ActivityPriorityChange     = "priority_change"
	ActivityAssignedUserChange = "assigned_user_change"
	ActivityAssignedTeamChange = "assigned_team_change"
	ActivitySelfAssign         = "self_assign"
	ActivityTagChange          = "tag_change"

	ContentTypeText = "text"
	ContentTypeHTML = "html"

	maxLastMessageLen  = 45
	maxMessagesPerPage = 30
)

// ListenAndDispatchMessages starts a pool of worker goroutines to handle
// message dispatching via inbox's channel and processes incoming messages. It scans for
// pending outgoing messages at the specified read interval and pushes them to the outgoing queue.
func (m *Manager) ListenAndDispatchMessages(ctx context.Context, dispatchConcurrency int, scanInterval time.Duration) {
	dbScanner := time.NewTicker(scanInterval)
	defer dbScanner.Stop()

	// Spawn a worker goroutine pool to dispatch messages.
	for range dispatchConcurrency {
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			m.MessageDispatchWorker(ctx)
		}()
	}

	// Spawn a goroutine to process incoming messages.
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.IncomingMessageWorker(ctx)
	}()

	// Scan pending outgoing messages and send them.
	for {
		select {
		case <-ctx.Done():
			return
		case <-dbScanner.C:
			var (
				pendingMessages = []models.Message{}
				messageIDs      = m.getOutgoingProcessingMessageIDs()
			)

			// Get pending outgoing messages and skip the currently processing message ids.
			if err := m.q.GetPendingMessages.Select(&pendingMessages, pq.Array(messageIDs)); err != nil {
				m.lo.Error("error fetching pending messages from db", "error", err)
				continue
			}

			// Prepare and push the message to the outgoing queue.
			for _, message := range pendingMessages {
				// Get inbox.
				inb, err := m.inboxStore.Get(message.InboxID)
				if err != nil {
					m.lo.Error("error fetching inbox", "message_id", message.ID, "inbox_id", message.InboxID, "error", err)
					continue
				}

				// Render content in template.
				if err := m.RenderContentInTemplate(inb, message); err != nil {
					m.lo.Error("error rendering content", "message_id", message.ID, "error", err)
					continue
				}

				// Put the message ID in the processing map.
				m.outgoingProcessingMessages.Store(message.ID, message.ID)

				// Push the message to the outgoing message queue.
				m.outgoingMessageQueue <- message
			}
		}
	}
}

// Close signals the Manager to stop processing messages, closes channels,
// and waits for all worker goroutines to finish processing.
func (m *Manager) Close() {
	m.closedMu.Lock()
	defer m.closedMu.Unlock()
	m.closed = true
	close(m.outgoingMessageQueue)
	close(m.incomingMessageQueue)
	m.wg.Wait()
}

// IncomingMessageWorker processes incoming messages from the incoming message queue.
func (m *Manager) IncomingMessageWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-m.incomingMessageQueue:
			if !ok {
				return
			}
			if err := m.processIncomingMessage(msg); err != nil {
				m.lo.Error("error processing incoming msg", "error", err)
			}
		}
	}
}

// MessageDispatchWorker dispatches outgoing pending messages.
func (m *Manager) MessageDispatchWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case message, ok := <-m.outgoingMessageQueue:
			if !ok {
				return
			}

			// Get inbox.
			inbox, err := m.inboxStore.Get(message.InboxID)
			if err != nil {
				m.lo.Error("error fetching inbox", "error", err, "inbox_id", message.InboxID)
				m.outgoingProcessingMessages.Delete(message.ID)
				m.UpdateMessageStatus(message.UUID, MessageStatusFailed)
				continue
			}

			// Attach attachments to the message.
			if err := m.attachAttachmentsToMessage(&message); err != nil {
				m.lo.Error("error attaching attachments to message", "error", err, "id", message.ID)
				m.outgoingProcessingMessages.Delete(message.ID)
				m.UpdateMessageStatus(message.UUID, MessageStatusFailed)
				continue
			}

			// Get from, to addresses and inReplyTo.
			message.From = inbox.FromAddress()
			message.To, _ = m.GetToAddress(message.ConversationID, inbox.Channel())
			message.InReplyTo, _ = m.GetLatestReceivedMessageSourceID(message.ConversationID)

			// Send.
			err = inbox.Send(message)

			// Update status.
			var newStatus = MessageStatusSent
			if err != nil {
				newStatus = MessageStatusFailed
				m.lo.Error("error sending message", "error", err, "inbox_id", message.InboxID)
			}
			m.UpdateMessageStatus(message.UUID, newStatus)

			// Update first reply at.
			if newStatus == MessageStatusSent {
				m.UpdateConversationFirstReplyAt(message.ConversationUUID, message.ConversationID, message.CreatedAt)
			}

			// Delete from processing map.
			m.outgoingProcessingMessages.Delete(message.ID)
		}
	}
}

// RenderContentInTemplate renders message content in the default template
func (m *Manager) RenderContentInTemplate(inb inbox.Inbox, message models.Message) error {
	var (
		channel = inb.Channel()
		err     error
	)
	switch channel {
	case inbox.ChannelEmail:
		message.Content, err = m.template.RenderDefault(map[string]string{
			"Content": message.Content,
		})
		if err != nil {
			m.lo.Error("could not render email content using template", "id", message.ID, "error", err)
			m.UpdateMessageStatus(message.UUID, MessageStatusFailed)
			return fmt.Errorf("could not render email content using template: %w", err)
		}
	default:
		m.lo.Warn("WARNING: unknown message channel", "channel", channel)
		m.UpdateMessageStatus(message.UUID, MessageStatusFailed)
		return fmt.Errorf("unknown message channel: %s", channel)
	}
	return nil
}

// GetConversationMessages retrieves messages for a specific conversation.
func (m *Manager) GetConversationMessages(conversationUUID string, page, pageSize int) ([]models.Message, error) {
	var (
		messages = make([]models.Message, 0)
		qArgs    []interface{}
	)

	qArgs = append(qArgs, conversationUUID)
	query, qArgs, err := m.generateMessagesQuery(m.q.GetMessages, qArgs, page, pageSize)
	if err != nil {
		m.lo.Error("error generating messages query", "error", err)
		return messages, envelope.NewError(envelope.GeneralError, "Error fetching messages", nil)
	}

	tx, err := m.db.BeginTxx(context.Background(), nil)
	defer tx.Rollback()
	if err != nil {
		m.lo.Error("error preparing get messages query", "error", err)
		return messages, envelope.NewError(envelope.GeneralError, "Error fetching messages", nil)
	}

	if err := tx.Select(&messages, query, qArgs...); err != nil {
		m.lo.Error("error fetching conversations", "error", err)
		return messages, envelope.NewError(envelope.GeneralError, "Error fetching messages", nil)
	}

	return messages, nil
}

// GetMessage retrieves a message by UUID.
func (m *Manager) GetMessage(uuid string) (models.Message, error) {
	var message models.Message
	if err := m.q.GetMessage.Get(&message, uuid); err != nil {
		m.lo.Error("error fetching message", "uuid", uuid, "error", err)
		return message, envelope.NewError(envelope.GeneralError, "Error fetching message", nil)
	}
	return message, nil
}

// UpdateMessageStatus updates the status of a message.
func (m *Manager) UpdateMessageStatus(uuid string, status string) error {
	if _, err := m.q.UpdateMessageStatus.Exec(status, uuid); err != nil {
		m.lo.Error("error updating message status", "error", err, "uuid", uuid)
		return err
	}

	// Broadcast messge status update to all conversation subscribers.
	conversationUUID, _ := m.getConversationUUIDFromMessageUUID(uuid)
	m.BroadcastMessagePropUpdate(conversationUUID, uuid, "status" /*property*/, status)
	return nil
}

// MarkMessageAsPending updates message status to `Pending`, so if it's a outgoing message it can be picked up again by a worker.
func (m *Manager) MarkMessageAsPending(uuid string) error {
	if err := m.UpdateMessageStatus(uuid, MessageStatusPending); err != nil {
		return envelope.NewError(envelope.GeneralError, "Error retrying message", nil)
	}
	return nil
}

// InsertMessage inserts a message and attaches the attachments to the message.
func (m *Manager) InsertMessage(message *models.Message) error {
	// Private message is always sent.
	if message.Private {
		message.Status = MessageStatusSent
	}

	if message.Meta == "" {
		message.Meta = "{}"
	}

	// Insert Message.
	if err := m.q.InsertMessage.QueryRow(message.Type, message.Status, message.ConversationID, message.ConversationUUID, message.Content, message.SenderID, message.SenderType,
		message.Private, message.ContentType, message.SourceID, message.InboxID, message.Meta).Scan(&message.ID, &message.UUID, &message.CreatedAt); err != nil {
		m.lo.Error("error inserting message in db", "error", err)
		return envelope.NewError(envelope.GeneralError, "Error sending message", nil)
	}

	// Attach message to the media.
	for _, media := range message.Media {
		m.mediaStore.Attach(media.ID, mmodels.ModelMessages, message.ID)
	}

	// Add this user as a participant.
	if err := m.AddConversationParticipant(message.SenderID, message.ConversationUUID); err != nil {
		return envelope.NewError(envelope.GeneralError, "Error sending message", nil)
	}

	// Update conversation meta with the last message details.
	trimmedMessage := stringutil.SanitizeAndTruncate(message.Content, 45)
	m.UpdateConversationLastMessage(0, message.ConversationUUID, trimmedMessage, message.CreatedAt)

	// Broadcast new message to all conversation subscribers.
	m.BroadcastNewConversationMessage(message.ConversationUUID, trimmedMessage, message.UUID, message.CreatedAt.Format(time.RFC3339), message.Type, message.Private)
	return nil
}

// RecordAssigneeUserChange records an activity for a user assignee change.
func (m *Manager) RecordAssigneeUserChange(conversationUUID string, assigneeID int, actor umodels.User) error {
	// Self assign.
	if assigneeID == actor.ID {
		return m.InsertConversationActivity(ActivitySelfAssign, conversationUUID, actor.FullName(), actor)
	}

	// Assignment to another user.
	assignee, err := m.userStore.Get(assigneeID)
	if err != nil {
		return err
	}
	return m.InsertConversationActivity(ActivityAssignedUserChange, conversationUUID, assignee.FullName(), actor)
}

// RecordAssigneeTeamChange records an activity for a team assignee change.
func (m *Manager) RecordAssigneeTeamChange(conversationUUID string, teamID int, actor umodels.User) error {
	team, err := m.teamStore.GetTeam(teamID)
	if err != nil {
		return err
	}
	return m.InsertConversationActivity(ActivityAssignedTeamChange, conversationUUID, team.Name, actor)
}

// RecordPriorityChange records an activity for a priority change.
func (m *Manager) RecordPriorityChange(priority, conversationUUID string, actor umodels.User) error {
	return m.InsertConversationActivity(ActivityPriorityChange, conversationUUID, priority, actor)
}

// RecordStatusChange records an activity for a status change.
func (m *Manager) RecordStatusChange(status, conversationUUID string, actor umodels.User) error {
	return m.InsertConversationActivity(ActivityStatusChange, conversationUUID, status, actor)
}

// InsertConversationActivity inserts an activity message.
func (m *Manager) InsertConversationActivity(activityType, conversationUUID, newValue string, actor umodels.User) error {
	content, err := m.getMessageActivityContent(activityType, newValue, actor.FullName())
	if err != nil {
		m.lo.Error("error could not generate activity content", "error", err)
		return err
	}

	message := models.Message{
		Type:             MessageActivity,
		Status:           MessageStatusSent,
		Content:          content,
		ContentType:      ContentTypeText,
		ConversationUUID: conversationUUID,
		Private:          true,
		SenderID:         actor.ID,
		SenderType:       SenderTypeUser,
	}

	// InsertMessage message in DB.
	m.InsertMessage(&message)
	return nil
}

// getConversationUUIDFromMessageUUID returns conversation UUID from message UUID.
func (m *Manager) getConversationUUIDFromMessageUUID(uuid string) (string, error) {
	var conversationUUID string
	if err := m.q.GetConversationUUIDFromMessageUUID.Get(&conversationUUID, uuid); err != nil {
		m.lo.Error("error fetching conversation uuid from message uuid", "uuid", uuid, "error", err)
		return conversationUUID, err
	}
	return conversationUUID, nil
}

// getMessageActivityContent generates activity content based on the activity type.
func (m *Manager) getMessageActivityContent(activityType, newValue, actorName string) (string, error) {
	var content = ""
	switch activityType {
	case ActivityAssignedUserChange:
		content = fmt.Sprintf("Assigned to %s by %s", newValue, actorName)
	case ActivityAssignedTeamChange:
		content = fmt.Sprintf("Assigned to %s team by %s", newValue, actorName)
	case ActivitySelfAssign:
		content = fmt.Sprintf("%s self-assigned this conversation", actorName)
	case ActivityPriorityChange:
		content = fmt.Sprintf("%s changed priority to %s", actorName, newValue)
	case ActivityStatusChange:
		content = fmt.Sprintf("%s marked the conversation as %s", actorName, newValue)
	case ActivityTagChange:
		content = fmt.Sprintf("%s added tags %s", actorName, newValue)
	default:
		return "", fmt.Errorf("invalid activity type %s", activityType)
	}
	return content, nil
}

// processIncomingMessage handles the insertion of an incoming message and
// associated contact. It finds or creates the contact, checks for existing
// conversations, and creates a new conversation if necessary. It also
// inserts the message, uploads any attachments, and evaluates automation
// rules for new conversations.
func (m *Manager) processIncomingMessage(in models.IncomingMessage) error {
	var err error

	// Find or create contact.
	in.Message.SenderID, err = m.contactStore.Upsert(in.Contact)
	if err != nil {
		m.lo.Error("error upserting contact", "error", err)
		return err
	}

	// This message already exists?
	conversationID, err := m.findConversationID([]string{in.Message.SourceID.String})
	if err != nil && err != ErrConversationNotFound {
		return err
	}
	if conversationID > 0 {
		return nil
	}

	// Find or create new conversation.
	isNewConversation, err := m.findOrCreateConversation(&in.Message, in.InboxID, in.Message.SenderID)
	if err != nil {
		return err
	}

	// Insert message.
	if err = m.InsertMessage(&in.Message); err != nil {
		return err
	}

	// Upload attachments.
	if err := m.uploadMessageAttachments(&in.Message); err != nil {
		return err
	}

	// Evaluate automation rules for this conversation.
	if isNewConversation {
		m.automation.EvaluateNewConversationRules(in.Message.ConversationUUID)
	}
	return nil
}

// MessageExists checks if a message with the given messageID exists.
func (m *Manager) MessageExists(messageID string) (bool, error) {
	_, err := m.findConversationID([]string{messageID})
	if err != nil {
		if errors.Is(err, ErrConversationNotFound) {
			return false, nil
		}
		m.lo.Error("error fetching message from db", "error", err)
		return false, err
	}
	return true, nil
}

// EnqueueIncoming enqueues an incoming message for inserting in db.
func (m *Manager) EnqueueIncoming(message models.IncomingMessage) error {
	m.closedMu.Lock()
	defer m.closedMu.Unlock()
	if m.closed {
		return errors.New("incoming message queue is closed")
	}

	select {
	case m.incomingMessageQueue <- message:
		return nil
	default:
		m.lo.Warn("WARNING: incoming message queue is full")
		return errors.New("incoming message queue is full")
	}
}

// GetConversationByMessageID returns conversation by message id.
func (m *Manager) GetConversationByMessageID(id int) (models.Conversation, error) {
	var conversation = models.Conversation{}
	if err := m.q.GetConversationByMessageID.Get(&conversation, id); err != nil {
		if err == sql.ErrNoRows {
			return conversation, ErrConversationNotFound
		}
		m.lo.Error("error fetching message from DB", "error", err)
		return conversation, envelope.NewError(envelope.GeneralError, "Error fetching message", nil)
	}
	return conversation, nil
}

// generateMessagesQuery generates the SQL query for fetching messages in a conversation.
func (c *Manager) generateMessagesQuery(baseQuery string, qArgs []interface{}, page, pageSize int) (string, []interface{}, error) {
	// Set default values for page and page size if they are invalid
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 || pageSize > maxMessagesPerPage {
		pageSize = maxMessagesPerPage
	}

	// Calculate the offset
	offset := (page - 1) * pageSize

	// Append LIMIT and OFFSET to query arguments
	qArgs = append(qArgs, pageSize, offset)

	// Include LIMIT and OFFSET in the SQL query
	sqlQuery := fmt.Sprintf(baseQuery, fmt.Sprintf("LIMIT $%d OFFSET $%d", len(qArgs)-1, len(qArgs)))
	return sqlQuery, qArgs, nil
}

// uploadMessageAttachments uploads attachments for a message.
func (m *Manager) uploadMessageAttachments(message *models.Message) error {
	for _, attachment := range message.Attachments {
		m.lo.Debug("uploading message attachment", "name", attachment.Name)
		reader := bytes.NewReader(attachment.Content)
		attachment.Name = stringutil.SanitizeFilename(attachment.Name)
		_, err := m.mediaStore.UploadAndInsert(attachment.Name, attachment.ContentType, attachment.ContentID, mmodels.ModelMessages, message.ID, reader, attachment.Size, attachment.Disposition, []byte("{}"))
		if err != nil {
			continue
		}
	}
	return nil
}

// findOrCreateConversation finds or creates a conversation for the given message.
func (m *Manager) findOrCreateConversation(in *models.Message, inboxID int, contactID int) (bool, error) {
	var (
		new              bool
		err              error
		conversationID   int
		conversationUUID string
	)

	// Search for existing conversation.
	sourceIDs := in.References
	if in.InReplyTo != "" {
		sourceIDs = append(sourceIDs, in.InReplyTo)
	}
	conversationID, err = m.findConversationID(sourceIDs)
	if err != nil && err != ErrConversationNotFound {
		return new, err
	}

	// Conversation not found, create one.
	if conversationID == 0 {
		new = true

		// Put subject & last message details in meta.
		conversationMeta, err := json.Marshal(map[string]string{
			"subject":         in.Subject,
			"last_message":    stringutil.SanitizeAndTruncate(in.Content, maxLastMessageLen),
			"last_message_at": time.Now().Format(time.RFC3339),
		})
		if err != nil {
			return false, err
		}
		conversationID, conversationUUID, err = m.CreateConversation(contactID, inboxID, conversationMeta)
		if err != nil || conversationID == 0 {
			return new, err
		}
		in.ConversationID = conversationID
		in.ConversationUUID = conversationUUID
		return new, nil
	}

	// Get UUID.
	if conversationUUID == "" {
		conversationUUID, err = m.GetConversationUUID(conversationID)
		if err != nil {
			return new, err
		}
	}
	in.ConversationID = conversationID
	in.ConversationUUID = conversationUUID
	return new, nil
}

// findConversationID finds the conversation ID from the message source ID.
func (m *Manager) findConversationID(messageSourceIDs []string) (int, error) {
	if len(messageSourceIDs) == 0 {
		return 0, ErrConversationNotFound
	}
	var conversationID int
	if err := m.q.MessageExistsBySourceID.QueryRow(pq.Array(messageSourceIDs)).Scan(&conversationID); err != nil {
		if err == sql.ErrNoRows {
			return conversationID, ErrConversationNotFound
		}
		m.lo.Error("error fetching msg from DB", "error", err)
		return conversationID, err
	}
	return conversationID, nil
}

// attachAttachmentsToMessage attaches attachment blobs to message.
func (m *Manager) attachAttachmentsToMessage(message *models.Message) error {
	var attachments attachment.Attachments

	// Get all media for this message.
	medias, err := m.mediaStore.GetByModel(message.ID, mmodels.ModelMessages)
	if err != nil {
		m.lo.Error("error fetching message attachments", "error", err)
		return err
	}

	// Fetch blobs.
	for _, media := range medias {
		blob, err := m.mediaStore.GetBlob(media.UUID)
		if err != nil {
			m.lo.Error("error fetching media blob", "error", err)
			return err
		}
		attachment := attachment.Attachment{
			Name:    media.Filename,
			Content: blob,
			Header:  attachment.MakeHeader(media.ContentType, media.UUID, media.Filename, "base64", media.Disposition),
		}
		attachments = append(attachments, attachment)
	}

	// Attach attachments.
	message.Attachments = attachments

	return nil
}

// getOutgoingProcessingMessageIDs returns the IDs of outgoing messages currently being processed.
func (m *Manager) getOutgoingProcessingMessageIDs() []int {
	var out = make([]int, 0)
	m.outgoingProcessingMessages.Range(func(key, _ any) bool {
		if k, ok := key.(int); ok {
			out = append(out, k)
		}
		return true
	})
	return out
}
