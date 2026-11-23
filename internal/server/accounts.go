package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/tatoalo/degitide/internal/db"
)

func (s *Server) requestAccount(w http.ResponseWriter, r *http.Request) (db.Account, bool) {
	id := r.URL.Query().Get("account")
	if id == "" {
		id = db.DefaultAccountID
	}
	var account db.Account
	if !db.ValidAccountID(id) {
		writeError(w, http.StatusBadRequest, errors.New("invalid account ID"))
		return account, false
	}
	if err := s.db.Where("id = ?", id).First(&account).Error; err != nil {
		writeError(w, http.StatusNotFound, errors.New("account not found"))
		return account, false
	}
	return account, true
}

func (s *Server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	accounts, err := s.db.Accounts()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, accounts)
}

func (s *Server) handleCreateAccount(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, errors.New("invalid account"))
		return
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		writeError(w, http.StatusBadRequest, errors.New("invalid account"))
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	if len(input.Name) < 1 || len(input.Name) > 80 || strings.ContainsAny(input.Name, "\r\n\x00") {
		writeError(w, http.StatusBadRequest, errors.New("enter an account name of 1–80 characters"))
		return
	}
	if input.ID == "" {
		var id [8]byte
		if _, err := rand.Read(id[:]); err != nil {
			writeError(w, 500, errors.New("could not create account"))
			return
		}
		input.ID = hex.EncodeToString(id[:])
	}
	if !db.ValidAccountID(input.ID) {
		writeError(w, http.StatusBadRequest, errors.New("account ID must contain lowercase letters, digits, underscores or hyphens"))
		return
	}
	account := db.Account{ID: input.ID, Name: input.Name}
	if err := s.db.Create(&account).Error; err != nil {
		writeError(w, http.StatusConflict, errors.New("account ID already exists"))
		return
	}
	writeJSONStatus(w, http.StatusCreated, account)
}
