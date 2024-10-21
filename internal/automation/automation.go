// Package automation automatically evaluates and applies rules to conversations based on events like new conversations, updates, and time triggers,
// and performs some actions if they are true.
package automation

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"sync"
	"time"

	"github.com/abhinavxd/artemis/internal/automation/models"
	cmodels "github.com/abhinavxd/artemis/internal/conversation/models"
	"github.com/abhinavxd/artemis/internal/dbutil"
	"github.com/abhinavxd/artemis/internal/envelope"
	umodels "github.com/abhinavxd/artemis/internal/user/models"
	"github.com/jmoiron/sqlx"
	"github.com/zerodha/logf"
)

var (
	//go:embed queries.sql
	efs embed.FS

	// MaxQueueSize defines the maximum size of the task queues.
	MaxQueueSize = 5000
)

// TaskType represents the type of conversation task.
type TaskType string

const (
	NewConversation    TaskType = "new"
	UpdateConversation TaskType = "update"
	TimeTrigger        TaskType = "time-trigger"
)

// ConversationTask represents a unit of work for processing conversations.
type ConversationTask struct {
	taskType         TaskType
	conversationUUID string
}

type Engine struct {
	rules               []models.Rule
	rulesMu             sync.RWMutex
	q                   queries
	lo                  *logf.Logger
	conversationStore   ConversationStore
	systemUser          umodels.User
	newConversationQ    chan string
	updateConversationQ chan string
	closed              bool
	closedMu            sync.RWMutex
	wg                  sync.WaitGroup
}

type Opts struct {
	DB *sqlx.DB
	Lo *logf.Logger
}

type ConversationStore interface {
	GetConversation(uuid string) (cmodels.Conversation, error)
	GetConversationsCreatedAfter(t time.Time) ([]cmodels.Conversation, error)
	UpdateConversationTeamAssignee(uuid string, teamID int, actor umodels.User) error
	UpdateConversationUserAssignee(uuid string, assigneeID int, actor umodels.User) error
	UpdateConversationStatus(uuid string, status []byte, actor umodels.User) error
	UpdateConversationPriority(uuid string, priority []byte, actor umodels.User) error
}

type queries struct {
	GetAll          *sqlx.Stmt `query:"get-all"`
	GetRule         *sqlx.Stmt `query:"get-rule"`
	InsertRule      *sqlx.Stmt `query:"insert-rule"`
	UpdateRule      *sqlx.Stmt `query:"update-rule"`
	DeleteRule      *sqlx.Stmt `query:"delete-rule"`
	ToggleRule      *sqlx.Stmt `query:"toggle-rule"`
	GetEnabledRules *sqlx.Stmt `query:"get-enabled-rules"`
}

// New initializes a new Engine.
func New(systemUser umodels.User, opt Opts) (*Engine, error) {
	var (
		q queries
		e = &Engine{
			systemUser:          systemUser,
			lo:                  opt.Lo,
			newConversationQ:    make(chan string, MaxQueueSize),
			updateConversationQ: make(chan string, MaxQueueSize),
		}
	)
	if err := dbutil.ScanSQLFile("queries.sql", &q, opt.DB, efs); err != nil {
		return nil, err
	}
	e.q = q
	e.rules = e.queryRules()
	return e, nil
}

// SetConversationStore sets conversations store.
func (e *Engine) SetConversationStore(store ConversationStore) {
	e.conversationStore = store
}

// ReloadRules reloads automation rules from DB.
func (e *Engine) ReloadRules() {
	e.rulesMu.Lock()
	defer e.rulesMu.Unlock()
	e.lo.Debug("reloading automation engine rules")
	e.rules = e.queryRules()
}

// Run starts the Engine with a worker pool to evaluate rules based on events.
func (e *Engine) Run(ctx context.Context, workerCount int) {
	e.wg.Add(workerCount)

	taskQueue := make(chan ConversationTask, MaxQueueSize)

	// Start the worker pool
	for i := 0; i < workerCount; i++ {
		go e.worker(ctx, taskQueue)
	}

	// Ticker for timed triggers.
	ticker := time.NewTicker(1 * time.Hour)
	defer func() {
		ticker.Stop()
		close(taskQueue)
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case conversationUUID, ok := <-e.newConversationQ:
			if !ok {
				return
			}
			e.lo.Info("queuing new conversation to evaluate rules", "uuid", conversationUUID)
			taskQueue <- ConversationTask{taskType: NewConversation, conversationUUID: conversationUUID}
		case conversationUUID, ok := <-e.updateConversationQ:
			if !ok {
				return
			}
			e.lo.Info("queuing conversation to evaluate rules on update", "uuid", conversationUUID)
			taskQueue <- ConversationTask{taskType: UpdateConversation, conversationUUID: conversationUUID}
		case <-ticker.C:
			e.lo.Info("queuing time triggers")
			taskQueue <- ConversationTask{taskType: TimeTrigger}
		}
	}
}

// worker processes tasks from the taskQueue until it's closed or context is done.
func (e *Engine) worker(ctx context.Context, taskQueue <-chan ConversationTask) {
	defer e.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case task, ok := <-taskQueue:
			if !ok {
				return
			}
			switch task.taskType {
			case NewConversation:
				e.handleNewConversation(task.conversationUUID)
			case UpdateConversation:
				e.handleUpdateConversation(task.conversationUUID)
			case TimeTrigger:
				e.handleTimeTrigger()
			}
		}
	}
}

// Close signals the Engine to stop accepting any more messages and waits for all workers to finish.
func (e *Engine) Close() {
	e.closedMu.Lock()
	defer e.closedMu.Unlock()
	if e.closed {
		return
	}
	e.closed = true
	close(e.newConversationQ)
	close(e.updateConversationQ)

	// Wait for all workers.
	e.wg.Wait()
}

// GetAllRules retrieves all rules of a specific type.
func (e *Engine) GetAllRules(typ []byte) ([]models.RuleRecord, error) {
	var rules = make([]models.RuleRecord, 0)
	if err := e.q.GetAll.Select(&rules, typ); err != nil {
		e.lo.Error("error fetching rules", "error", err)
		return rules, envelope.NewError(envelope.GeneralError, "Error fetching automation rules.", nil)
	}
	return rules, nil
}

// GetRule retrieves a rule by ID.
func (e *Engine) GetRule(id int) (models.RuleRecord, error) {
	var rule models.RuleRecord
	if err := e.q.GetRule.Get(&rule, id); err != nil {
		if err == sql.ErrNoRows {
			return rule, envelope.NewError(envelope.InputError, "Rule not found.", nil)
		}
		e.lo.Error("error fetching rule", "error", err)
		return rule, envelope.NewError(envelope.GeneralError, "Error fetching automation rule.", nil)
	}
	return rule, nil
}

// ToggleRule toggles the active status of a rule by ID.
func (e *Engine) ToggleRule(id int) error {
	if _, err := e.q.ToggleRule.Exec(id); err != nil {
		e.lo.Error("error toggling rule", "error", err)
		return envelope.NewError(envelope.GeneralError, "Error toggling automation rule.", nil)
	}
	// Reload rules.
	e.ReloadRules()
	return nil
}

// UpdateRule updates an existing rule.
func (e *Engine) UpdateRule(id int, rule models.RuleRecord) error {
	if _, err := e.q.UpdateRule.Exec(id, rule.Name, rule.Description, rule.Type, rule.Rules); err != nil {
		e.lo.Error("error updating rule", "error", err)
		return envelope.NewError(envelope.GeneralError, "Error updating automation rule.", nil)
	}
	// Reload rules.
	e.ReloadRules()
	return nil
}

// CreateRule creates a new rule.
func (e *Engine) CreateRule(rule models.RuleRecord) error {
	if _, err := e.q.InsertRule.Exec(rule.Name, rule.Description, rule.Type, rule.Rules); err != nil {
		e.lo.Error("error creating rule", "error", err)
		return envelope.NewError(envelope.GeneralError, "Error creating automation rule.", nil)
	}
	// Reload rules.
	e.ReloadRules()
	return nil
}

// DeleteRule deletes a rule by ID.
func (e *Engine) DeleteRule(id int) error {
	if _, err := e.q.DeleteRule.Exec(id); err != nil {
		e.lo.Error("error deleting rule", "error", err)
		return envelope.NewError(envelope.GeneralError, "Error deleting automation rule.", nil)
	}
	// Reload rules.
	e.ReloadRules()
	return nil
}

// handleNewConversation handles new conversation events.
func (e *Engine) handleNewConversation(conversationUUID string) {
	conversation, err := e.conversationStore.GetConversation(conversationUUID)
	if err != nil {
		e.lo.Error("error fetching conversation for new event", "uuid", conversationUUID, "error", err)
		return
	}
	rules := e.filterRulesByType(string(models.RuleTypeNewConversation))
	e.evalConversationRules(rules, conversation)
}

// handleUpdateConversation handles update conversation events.
func (e *Engine) handleUpdateConversation(conversationUUID string) {
	conversation, err := e.conversationStore.GetConversation(conversationUUID)
	if err != nil {
		e.lo.Error("error fetching conversation for update event", "uuid", conversationUUID, "error", err)
		return
	}
	rules := e.filterRulesByType(string(models.RuleTypeConversationUpdate))
	e.evalConversationRules(rules, conversation)
}

// handleTimeTrigger handles time trigger events.
func (e *Engine) handleTimeTrigger() {
	thirtyDaysAgo := time.Now().Add(-30 * 24 * time.Hour)
	conversations, err := e.conversationStore.GetConversationsCreatedAfter(thirtyDaysAgo)
	if err != nil {
		e.lo.Error("error fetching conversations for time trigger", "error", err)
		return
	}
	rules := e.filterRulesByType(string(models.RuleTypeTimeTrigger))
	e.lo.Debug("fetched conversations for evaluating time triggers", "conversations_count", len(conversations), "rules_count", len(rules))
	for _, conversation := range conversations {
		e.evalConversationRules(rules, conversation)
	}
}

// EvaluateNewConversationRules enqueues a new conversation for rule evaluation.
func (e *Engine) EvaluateNewConversationRules(conversationUUID string) {
	e.closedMu.RLock()
	defer e.closedMu.RUnlock()
	if e.closed {
		return
	}
	select {
	case e.newConversationQ <- conversationUUID:
	default:
		// Queue is full.
		e.lo.Warn("EvaluateNewConversationRules: newConversationQ is full, unable to enqueue conversation")
	}
}

// EvaluateConversationUpdateRules enqueues an updated conversation for rule evaluation.
func (e *Engine) EvaluateConversationUpdateRules(conversationUUID string) {
	e.closedMu.RLock()
	defer e.closedMu.RUnlock()
	if e.closed {
		return
	}
	select {
	case e.updateConversationQ <- conversationUUID:
	default:
		// Queue is full.
		e.lo.Warn("EvaluateConversationUpdateRules: updateConversationQ is full, unable to enqueue conversation")
	}
}

// queryRules fetches automation rules from the database.
func (e *Engine) queryRules() []models.Rule {
	var (
		rules []struct {
			Type  string `db:"type"`
			Rules string `db:"rules"`
		}
		filteredRules []models.Rule
	)
	err := e.q.GetEnabledRules.Select(&rules)
	if err != nil {
		e.lo.Error("error fetching automation rules", "error", err)
		return filteredRules
	}

	for _, rule := range rules {
		var rulesBatch []models.Rule
		if err := json.Unmarshal([]byte(rule.Rules), &rulesBatch); err != nil {
			e.lo.Error("error unmarshalling rule JSON", "error", err)
			continue
		}
		// Set the Type for each rule in rulesBatch
		for i := range rulesBatch {
			rulesBatch[i].Type = rule.Type
		}
		filteredRules = append(filteredRules, rulesBatch...)
	}
	return filteredRules
}

// filterRulesByType filters rules by type.
func (e *Engine) filterRulesByType(ruleType string) []models.Rule {
	e.rulesMu.RLock()
	defer e.rulesMu.RUnlock()

	var filteredRules []models.Rule
	for _, rule := range e.rules {
		if rule.Type == ruleType {
			filteredRules = append(filteredRules, rule)
		}
	}
	return filteredRules
}
