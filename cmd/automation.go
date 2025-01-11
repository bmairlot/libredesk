package main

import (
	"strconv"

	amodels "github.com/abhinavxd/libredesk/internal/automation/models"
	"github.com/abhinavxd/libredesk/internal/envelope"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
)

func handleGetAutomationRules(r *fastglue.Request) error {
	var (
		app = r.Context.(*App)
		typ = r.RequestCtx.QueryArgs().Peek("type")
	)
	out, err := app.automation.GetAllRules(typ)
	if err != nil {
		return sendErrorEnvelope(r, err)
	}
	return r.SendEnvelope(out)
}

func handleGetAutomationRule(r *fastglue.Request) error {
	var (
		app   = r.Context.(*App)
		id, _ = strconv.Atoi(r.RequestCtx.UserValue("id").(string))
	)
	out, err := app.automation.GetRule(id)
	if err != nil {
		return sendErrorEnvelope(r, err)
	}
	return r.SendEnvelope(out)
}

func handleToggleAutomationRule(r *fastglue.Request) error {
	var (
		app   = r.Context.(*App)
		id, _ = strconv.Atoi(r.RequestCtx.UserValue("id").(string))
	)
	if err := app.automation.ToggleRule(id); err != nil {
		return sendErrorEnvelope(r, err)
	}
	return r.SendEnvelope(true)
}

func handleUpdateAutomationRule(r *fastglue.Request) error {
	var (
		app     = r.Context.(*App)
		rule    = amodels.RuleRecord{}
		id, err = strconv.Atoi(r.RequestCtx.UserValue("id").(string))
	)
	if err != nil || id == 0 {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest,
			"Invalid rule `id`.", nil, envelope.InputError)
	}

	if err := r.Decode(&rule, "json"); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "decode failed", nil, envelope.InputError)
	}

	err = app.automation.UpdateRule(id, rule)
	if err != nil {
		return sendErrorEnvelope(r, err)
	}
	return r.SendEnvelope(true)
}

func handleCreateAutomationRule(r *fastglue.Request) error {
	var (
		app  = r.Context.(*App)
		rule = amodels.RuleRecord{}
	)
	if err := r.Decode(&rule, "json"); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "decode failed", nil, envelope.InputError)
	}
	err := app.automation.CreateRule(rule)
	if err != nil {
		return sendErrorEnvelope(r, err)
	}
	return r.SendEnvelope(true)
}

func handleDeleteAutomationRule(r *fastglue.Request) error {
	var (
		app = r.Context.(*App)

		id, err = strconv.Atoi(r.RequestCtx.UserValue("id").(string))
	)
	if err != nil || id == 0 {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest,
			"Invalid rule `id`.", nil, envelope.InputError)
	}

	err = app.automation.DeleteRule(id)
	if err != nil {
		return sendErrorEnvelope(r, err)
	}
	return r.SendEnvelope(true)
}
