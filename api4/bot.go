// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

package api4

import (
	"bytes"
	"encoding/json"
	"io/ioutil"
	"net/http"
	"strconv"

	"github.com/mattermost/mattermost-server/v6/audit"
	"github.com/mattermost/mattermost-server/v6/model"
	"github.com/mattermost/mattermost-server/v6/shared/mlog"
)

func (api *API) InitBot() {
	api.BaseRoutes.Bots.Handle("", api.APISessionRequired(createBot)).Methods("POST")
	api.BaseRoutes.Bot.Handle("", api.APISessionRequired(patchBot)).Methods("PUT")
	api.BaseRoutes.Bot.Handle("", api.APISessionRequired(getBot)).Methods("GET")
	api.BaseRoutes.Bots.Handle("", api.APISessionRequired(getBots)).Methods("GET")
	api.BaseRoutes.Bot.Handle("/disable", api.APISessionRequired(disableBot)).Methods("POST")
	api.BaseRoutes.Bot.Handle("/enable", api.APISessionRequired(enableBot)).Methods("POST")
	api.BaseRoutes.Bot.Handle("/convert_to_user", api.APISessionRequired(convertBotToUser)).Methods("POST")
	api.BaseRoutes.Bot.Handle("/assign/{user_id:[A-Za-z0-9]+}", api.APISessionRequired(assignBot)).Methods("POST")
}

func createBot(c *Context, w http.ResponseWriter, r *http.Request) {
	postBody, readErr := ioutil.ReadAll(r.Body)
	if readErr != nil {
		return
	}
	defer r.Body.Close()

	var postPayload interface{}
	_ = json.NewDecoder(bytes.NewBuffer(postBody)).Decode(&postPayload)

	var botPatch *model.BotPatch
	err := json.NewDecoder(bytes.NewBuffer(postBody)).Decode(&botPatch)
	if err != nil {
		c.SetInvalidParam("bot")
		return
	}

	bot := &model.Bot{
		OwnerId: c.AppContext.Session().UserId,
	}
	bot.Patch(botPatch)

	auditRec := c.MakeAuditRecord("createBot", audit.Fail)
	defer c.LogAuditRec(auditRec)
	auditRec.AddMeta("bot", bot)

	if !c.App.SessionHasPermissionTo(*c.AppContext.Session(), model.PermissionCreateBot) {
		c.SetPermissionError(model.PermissionCreateBot)
		return
	}

	if user, err := c.App.GetUser(c.AppContext.Session().UserId); err == nil {
		if user.IsBot {
			c.SetPermissionError(model.PermissionCreateBot)
			return
		}
	}

	if !*c.App.Config().ServiceSettings.EnableBotAccountCreation {
		c.Err = model.NewAppError("createBot", "api.bot.create_disabled", nil, "", http.StatusForbidden)
		return
	}

	createdBot, appErr := c.App.CreateBot(c.AppContext, bot)
	if appErr != nil {
		c.Err = appErr
		return
	}

	auditRec.Success()
	auditRec.AddMeta("bot", createdBot) // overwrite meta
	auditRec.AddMetadata(postPayload, nil, bot, "bot")

	w.WriteHeader(http.StatusCreated)
	if err := json.NewEncoder(w).Encode(createdBot); err != nil {
		mlog.Warn("Error while writing response", mlog.Err(err))
	}
}

func patchBot(c *Context, w http.ResponseWriter, r *http.Request) {
	c.RequireBotUserId()
	if c.Err != nil {
		return
	}
	botUserId := c.Params.BotUserId

	postBody, readErr := ioutil.ReadAll(r.Body)
	if readErr != nil {
		return
	}
	defer r.Body.Close()

	var postPayload interface{}
	_ = json.NewDecoder(bytes.NewBuffer(postBody)).Decode(&postPayload)

	var botPatch *model.BotPatch
	err := json.NewDecoder(bytes.NewBuffer(postBody)).Decode(&botPatch)
	if err != nil {
		c.SetInvalidParam("bot")
		return
	}

	auditRec := c.MakeAuditRecord("patchBot", audit.Fail)
	defer c.LogAuditRec(auditRec)
	auditRec.AddMeta("bot_id", botUserId)

	if err := c.App.SessionHasPermissionToManageBot(*c.AppContext.Session(), botUserId); err != nil {
		c.Err = err
		return
	}

	oldBot, _ := c.App.GetBot(botUserId, false)
	updatedBot, appErr := c.App.PatchBot(botUserId, botPatch)
	if appErr != nil {
		c.Err = appErr
		return
	}

	auditRec.Success()
	auditRec.AddMeta("bot", updatedBot)
	auditRec.AddMetadata(postPayload, oldBot, updatedBot, "bot")

	if err := json.NewEncoder(w).Encode(updatedBot); err != nil {
		mlog.Warn("Error while writing response", mlog.Err(err))
	}
}

func getBot(c *Context, w http.ResponseWriter, r *http.Request) {
	c.RequireBotUserId()
	if c.Err != nil {
		return
	}
	botUserId := c.Params.BotUserId

	includeDeleted, _ := strconv.ParseBool(r.URL.Query().Get("include_deleted"))

	bot, appErr := c.App.GetBot(botUserId, includeDeleted)
	if appErr != nil {
		c.Err = appErr
		return
	}

	if c.App.SessionHasPermissionTo(*c.AppContext.Session(), model.PermissionReadOthersBots) {
		// Allow access to any bot.
	} else if bot.OwnerId == c.AppContext.Session().UserId {
		if !c.App.SessionHasPermissionTo(*c.AppContext.Session(), model.PermissionReadBots) {
			// Pretend like the bot doesn't exist at all to avoid revealing that the
			// user is a bot. It's kind of silly in this case, sine we created the bot,
			// but we don't have read bot permissions.
			c.Err = model.MakeBotNotFoundError(botUserId)
			return
		}
	} else {
		// Pretend like the bot doesn't exist at all, to avoid revealing that the
		// user is a bot.
		c.Err = model.MakeBotNotFoundError(botUserId)
		return
	}

	if c.HandleEtag(bot.Etag(), "Get Bot", w, r) {
		return
	}

	if err := json.NewEncoder(w).Encode(bot); err != nil {
		mlog.Warn("Error while writing response", mlog.Err(err))
	}
}

func getBots(c *Context, w http.ResponseWriter, r *http.Request) {
	includeDeleted, _ := strconv.ParseBool(r.URL.Query().Get("include_deleted"))
	onlyOrphaned, _ := strconv.ParseBool(r.URL.Query().Get("only_orphaned"))

	var OwnerId string
	if c.App.SessionHasPermissionTo(*c.AppContext.Session(), model.PermissionReadOthersBots) {
		// Get bots created by any user.
		OwnerId = ""
	} else if c.App.SessionHasPermissionTo(*c.AppContext.Session(), model.PermissionReadBots) {
		// Only get bots created by this user.
		OwnerId = c.AppContext.Session().UserId
	} else {
		c.SetPermissionError(model.PermissionReadBots)
		return
	}

	bots, appErr := c.App.GetBots(&model.BotGetOptions{
		Page:           c.Params.Page,
		PerPage:        c.Params.PerPage,
		OwnerId:        OwnerId,
		IncludeDeleted: includeDeleted,
		OnlyOrphaned:   onlyOrphaned,
	})
	if appErr != nil {
		c.Err = appErr
		return
	}

	if c.HandleEtag(bots.Etag(), "Get Bots", w, r) {
		return
	}

	if err := json.NewEncoder(w).Encode(bots); err != nil {
		mlog.Warn("Error while writing response", mlog.Err(err))
	}
}

func disableBot(c *Context, w http.ResponseWriter, _ *http.Request) {
	updateBotActive(c, w, false)
}

func enableBot(c *Context, w http.ResponseWriter, _ *http.Request) {
	updateBotActive(c, w, true)
}

func updateBotActive(c *Context, w http.ResponseWriter, active bool) {
	c.RequireBotUserId()
	if c.Err != nil {
		return
	}
	botUserId := c.Params.BotUserId

	auditRec := c.MakeAuditRecord("updateBotActive", audit.Fail)
	defer c.LogAuditRec(auditRec)
	auditRec.AddMeta("bot_id", botUserId)
	auditRec.AddMeta("enable", active)

	if err := c.App.SessionHasPermissionToManageBot(*c.AppContext.Session(), botUserId); err != nil {
		c.Err = err
		return
	}

	oldBot, _ := c.App.GetBot(botUserId, false)

	bot, err := c.App.UpdateBotActive(c.AppContext, botUserId, active)
	if err != nil {
		c.Err = err
		return
	}

	auditRec.Success()
	auditRec.AddMeta("bot", bot)
	auditRec.AddMetadata(c.Params, oldBot, bot, "bot")

	if err := json.NewEncoder(w).Encode(bot); err != nil {
		mlog.Warn("Error while writing response", mlog.Err(err))
	}
}

func assignBot(c *Context, w http.ResponseWriter, _ *http.Request) {
	c.RequireUserId()
	c.RequireBotUserId()
	if c.Err != nil {
		return
	}
	botUserId := c.Params.BotUserId
	userId := c.Params.UserId

	auditRec := c.MakeAuditRecord("assignBot", audit.Fail)
	defer c.LogAuditRec(auditRec)
	auditRec.AddMeta("bot_id", botUserId)
	auditRec.AddMeta("assign_user_id", userId)

	if err := c.App.SessionHasPermissionToManageBot(*c.AppContext.Session(), botUserId); err != nil {
		c.Err = err
		return
	}

	if user, err := c.App.GetUser(userId); err == nil {
		if user.IsBot {
			c.SetPermissionError(model.PermissionAssignBot)
			return
		}
	}

	bot, err := c.App.UpdateBotOwner(botUserId, userId)
	if err != nil {
		c.Err = err
		return
	}

	auditRec.Success()
	auditRec.AddMeta("bot", bot)
	auditRec.AddMetadata(c.Params, nil, bot, "bot")

	if err := json.NewEncoder(w).Encode(bot); err != nil {
		mlog.Warn("Error while writing response", mlog.Err(err))
	}
}

func convertBotToUser(c *Context, w http.ResponseWriter, r *http.Request) {
	c.RequireBotUserId()
	if c.Err != nil {
		return
	}

	bot, err := c.App.GetBot(c.Params.BotUserId, false)
	if err != nil {
		c.Err = err
		return
	}

	postBody, readErr := ioutil.ReadAll(r.Body)
	if readErr != nil {
		return
	}
	defer r.Body.Close()

	var postPayload interface{}
	_ = json.NewDecoder(bytes.NewBuffer(postBody)).Decode(&postPayload)

	var userPatch model.UserPatch
	jsonErr := json.NewDecoder(bytes.NewBuffer(postBody)).Decode(&userPatch)
	if jsonErr != nil || userPatch.Password == nil || *userPatch.Password == "" {
		c.SetInvalidParam("userPatch")
		return
	}

	systemAdmin, _ := strconv.ParseBool(r.URL.Query().Get("set_system_admin"))

	auditRec := c.MakeAuditRecord("convertBotToUser", audit.Fail)
	defer c.LogAuditRec(auditRec)
	auditRec.AddMeta("bot", bot)
	auditRec.AddMeta("userPatch", userPatch)
	auditRec.AddMeta("set_system_admin", systemAdmin)

	if !c.App.SessionHasPermissionTo(*c.AppContext.Session(), model.PermissionManageSystem) {
		c.SetPermissionError(model.PermissionManageSystem)
		return
	}

	user, err := c.App.ConvertBotToUser(bot, &userPatch, systemAdmin)
	if err != nil {
		c.Err = err
		return
	}

	auditRec.Success()
	auditRec.AddMeta("convertedTo", user)
	auditRec.AddMetadata(userPatch, bot, user, "user")

	if err := json.NewEncoder(w).Encode(user); err != nil {
		mlog.Warn("Error while writing response", mlog.Err(err))
	}
}
