package main

import (
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	amodels "github.com/abhinavxd/artemis/internal/auth/models"
	"github.com/abhinavxd/artemis/internal/envelope"
	"github.com/abhinavxd/artemis/internal/image"
	mmodels "github.com/abhinavxd/artemis/internal/media/models"
	notifier "github.com/abhinavxd/artemis/internal/notification"
	"github.com/abhinavxd/artemis/internal/stringutil"
	tmpl "github.com/abhinavxd/artemis/internal/template"
	"github.com/abhinavxd/artemis/internal/user/models"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
)

const (
	maxAvatarSizeMB = 5
)

// handleGetUsers returns all users.
func handleGetUsers(r *fastglue.Request) error {
	var (
		app = r.Context.(*App)
	)
	agents, err := app.user.GetAll()
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, err.Error(), nil, "")
	}
	return r.SendEnvelope(agents)
}

func handleGetUsersCompact(r *fastglue.Request) error {
	var (
		app = r.Context.(*App)
	)
	agents, err := app.user.GetAllCompact()
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, err.Error(), nil, "")
	}
	return r.SendEnvelope(agents)
}

func handleGetUser(r *fastglue.Request) error {
	var (
		app = r.Context.(*App)
	)
	id, err := strconv.Atoi(r.RequestCtx.UserValue("id").(string))
	if err != nil || id == 0 {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest,
			"Invalid user `id`.", nil, envelope.InputError)
	}
	user, err := app.user.Get(id)
	if err != nil {
		return sendErrorEnvelope(r, err)
	}
	return r.SendEnvelope(user)
}

// handleGetCurrentUserTeams returns the teams of a user.
func handleGetCurrentUserTeams(r *fastglue.Request) error {
	var (
		app   = r.Context.(*App)
		auser = r.RequestCtx.UserValue("user").(amodels.User)
	)
	user, err := app.user.Get(auser.ID)
	if err != nil {
		return sendErrorEnvelope(r, err)
	}

	teams, err := app.team.GetUserTeams(user.ID)
	if err != nil {
		return sendErrorEnvelope(r, err)
	}
	return r.SendEnvelope(teams)
}

func handleUpdateCurrentUser(r *fastglue.Request) error {
	var (
		app   = r.Context.(*App)
		auser = r.RequestCtx.UserValue("user").(amodels.User)
	)
	user, err := app.user.Get(auser.ID)
	if err != nil {
		return sendErrorEnvelope(r, err)
	}

	// Get current user.
	currentUser, err := app.user.Get(user.ID)
	if err != nil {
		return sendErrorEnvelope(r, err)
	}

	form, err := r.RequestCtx.MultipartForm()
	if err != nil {
		app.lo.Error("error parsing form data", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Error parsing data", nil, envelope.GeneralError)
	}

	files, ok := form.File["files"]

	// Upload avatar?
	if ok && len(files) > 0 {
		fileHeader := files[0]
		file, err := fileHeader.Open()
		if err != nil {
			app.lo.Error("error reading uploaded", "error", err)
			return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Error reading file", nil, envelope.GeneralError)
		}
		defer file.Close()

		// Sanitize filename.
		srcFileName := stringutil.SanitizeFilename(fileHeader.Filename)
		srcContentType := fileHeader.Header.Get("Content-Type")
		srcFileSize := fileHeader.Size
		srcExt := strings.TrimPrefix(strings.ToLower(filepath.Ext(srcFileName)), ".")

		if !slices.Contains(image.Exts, srcExt) {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "File type is not an image", nil, envelope.InputError)
		}

		// Check file size
		if bytesToMegabytes(srcFileSize) > maxAvatarSizeMB {
			app.lo.Error("error uploaded file size is larger than max allowed", "size", bytesToMegabytes(srcFileSize), "max_allowed", maxAvatarSizeMB)
			return r.SendErrorEnvelope(
				http.StatusRequestEntityTooLarge,
				fmt.Sprintf("File size is too large. Please upload file lesser than %d MB", maxAvatarSizeMB),
				nil,
				envelope.GeneralError,
			)
		}

		// Reset ptr.
		file.Seek(0, 0)
		media, err := app.media.UploadAndInsert(srcFileName, srcContentType, "", mmodels.ModelUser, user.ID, file, int(srcFileSize), "", []byte("{}"))
		if err != nil {
			app.lo.Error("error uploading file", "error", err)
			return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Error uploading file", nil, envelope.GeneralError)
		}

		// Delete current avatar.
		if currentUser.AvatarURL.Valid {
			fileName := filepath.Base(currentUser.AvatarURL.String)
			app.media.Delete(fileName)
		}

		// Save file path.
		path, err := stringutil.GetPathFromURL(media.URL)
		if err != nil {
			app.lo.Debug("error getting path from URL", "url", media.URL, "error", err)
			return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Error uploading file", nil, envelope.GeneralError)
		}
		if err := app.user.UpdateAvatar(user.ID, path); err != nil {
			return sendErrorEnvelope(r, err)
		}
	}

	return r.SendEnvelope(true)
}

// handleCreateUser creates a new user.
func handleCreateUser(r *fastglue.Request) error {
	var (
		app  = r.Context.(*App)
		user = models.User{}
	)
	if err := r.Decode(&user, "json"); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "decode failed", err.Error(), envelope.InputError)
	}

	if user.Email.String == "" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Empty `email`", nil, envelope.InputError)
	}

	err := app.user.CreateAgent(&user)
	if err != nil {
		return sendErrorEnvelope(r, err)
	}

	// Upsert user teams.
	if err := app.team.UpsertUserTeams(user.ID, user.Teams.Names()); err != nil {
		return sendErrorEnvelope(r, err)
	}

	if user.SendWelcomeEmail {
		// Generate reset token.
		resetToken, err := app.user.SetResetPasswordToken(user.ID)
		if err != nil {
			return sendErrorEnvelope(r, err)
		}

		// Render template and send email.
		content, err := app.tmpl.RenderTemplate(tmpl.TmplWelcome, map[string]interface{}{
			"ResetToken": resetToken,
			"Email":      user.Email,
		})
		if err != nil {
			app.lo.Error("error rendering template", "error", err)
			return r.SendEnvelope(true)
		}

		if err := app.notifier.Send(notifier.Message{
			UserIDs:  []int{user.ID},
			Subject:  "Welcome",
			Content:  content,
			Provider: notifier.ProviderEmail,
		}); err != nil {
			app.lo.Error("error sending notification message", "error", err)
			return r.SendEnvelope(true)
		}
	}
	return r.SendEnvelope(true)
}

// handleUpdateUser updates a user.
func handleUpdateUser(r *fastglue.Request) error {
	var (
		app  = r.Context.(*App)
		user = models.User{}
	)
	id, err := strconv.Atoi(r.RequestCtx.UserValue("id").(string))
	if err != nil || id == 0 {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest,
			"Invalid user `id`.", nil, envelope.InputError)
	}

	if err := r.Decode(&user, "json"); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "decode failed", err.Error(), envelope.InputError)
	}

	// Update user.
	err = app.user.Update(id, user)
	if err != nil {
		return sendErrorEnvelope(r, err)
	}

	// Upsert user teams.
	if err := app.team.UpsertUserTeams(id, user.Teams.Names()); err != nil {
		return sendErrorEnvelope(r, err)
	}

	return r.SendEnvelope(true)
}

// handleDeleteUser deletes a user.
func handleDeleteUser(r *fastglue.Request) error {
	var (
		app = r.Context.(*App)
	)
	id, err := strconv.Atoi(r.RequestCtx.UserValue("id").(string))
	if err != nil || id == 0 {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest,
			"Invalid user `id`.", nil, envelope.InputError)
	}

	// Soft delete user.
	err = app.user.SoftDelete(id)
	if err != nil {
		return sendErrorEnvelope(r, err)
	}

	// Unassign all open conversations assigned to the user.
	if err := app.conversation.UnassignOpen(id); err != nil {
		return sendErrorEnvelope(r, err)
	}

	return r.SendEnvelope(true)
}

// handleGetCurrentUser returns the current logged in user.
func handleGetCurrentUser(r *fastglue.Request) error {
	var (
		app   = r.Context.(*App)
		auser = r.RequestCtx.UserValue("user").(amodels.User)
	)
	user, err := app.user.Get(auser.ID)
	if err != nil {
		return sendErrorEnvelope(r, err)
	}
	u, err := app.user.Get(user.ID)
	if err != nil {
		return sendErrorEnvelope(r, err)
	}
	return r.SendEnvelope(u)
}

// handleDeleteAvatar deletes a user avatar.
func handleDeleteAvatar(r *fastglue.Request) error {
	var (
		app   = r.Context.(*App)
		auser = r.RequestCtx.UserValue("user").(amodels.User)
	)

	// Get user
	user, err := app.user.Get(auser.ID)
	if err != nil {
		return sendErrorEnvelope(r, err)
	}

	// Valid str?
	if user.AvatarURL.String == "" {
		return r.SendEnvelope(true)
	}

	fileName := filepath.Base(user.AvatarURL.String)

	// Delete file from the store.
	if err := app.media.Delete(fileName); err != nil {
		return sendErrorEnvelope(r, err)
	}
	err = app.user.UpdateAvatar(user.ID, "")
	if err != nil {
		return sendErrorEnvelope(r, err)
	}
	return r.SendEnvelope(true)
}

// handleResetPassword generates a reset password token and sends an email to the user.
func handleResetPassword(r *fastglue.Request) error {
	var (
		app       = r.Context.(*App)
		p         = r.RequestCtx.PostArgs()
		auser, ok = r.RequestCtx.UserValue("user").(amodels.User)
		email     = string(p.Peek("email"))
	)
	if ok && auser.ID > 0 {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "User is already logged in", nil, envelope.InputError)
	}

	if email == "" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Empty `email`", nil, envelope.InputError)
	}

	user, err := app.user.GetByEmail(email)
	if err != nil {
		return sendErrorEnvelope(r, err)
	}

	token, err := app.user.SetResetPasswordToken(user.ID)
	if err != nil {
		return sendErrorEnvelope(r, err)
	}

	// Send email.
	content, err := app.tmpl.RenderTemplate(tmpl.TmplResetPassword,
		map[string]string{
			"ResetToken": token,
		})
	if err != nil {
		app.lo.Error("error rendering template", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Error rendering template", nil, envelope.GeneralError)
	}

	if err := app.notifier.Send(notifier.Message{
		UserIDs:  []int{user.ID},
		Subject:  "Reset Password",
		Content:  content,
		Provider: notifier.ProviderEmail,
	}); err != nil {
		app.lo.Error("error sending notification message", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Error sending notification message", nil, envelope.GeneralError)
	}

	return r.SendEnvelope(true)
}

// handleSetPassword resets the password with the provided token.
func handleSetPassword(r *fastglue.Request) error {
	var (
		app      = r.Context.(*App)
		user, ok = r.RequestCtx.UserValue("user").(amodels.User)
		p        = r.RequestCtx.PostArgs()
		password = string(p.Peek("password"))
		token    = string(p.Peek("token"))
	)

	if ok && user.ID > 0 {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "User is already logged in", nil, envelope.InputError)
	}

	if password == "" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Empty `password`", nil, envelope.InputError)
	}

	if err := app.user.ResetPassword(token, password); err != nil {
		return sendErrorEnvelope(r, err)
	}

	return r.SendEnvelope(true)
}
