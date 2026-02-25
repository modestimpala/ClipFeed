package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"clipfeed/db"
	"clipfeed/httputil"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

const maxPasswordLen = 72 // bcrypt truncates at 72 bytes

type contextKey string

// UserIDKey is the context key used to store the authenticated user ID.
const UserIDKey contextKey = "user_id"

// ExtractUserID returns the user ID from the request context, if present.
func ExtractUserID(r *http.Request) (string, bool) {
	uid, ok := r.Context().Value(UserIDKey).(string)
	return uid, ok && uid != ""
}

// Handler holds dependencies for authentication endpoints.
type Handler struct {
	DB        *db.CompatDB
	JWTSecret string
}

// RegisterRequest is the JSON body for POST /api/auth/register.
type RegisterRequest struct {
	Username string `json:"username"`
	Email    string `json:"email"`
	Password string `json:"password"`
}

// HandleRegister creates a new user account.
func (h *Handler) HandleRegister(w http.ResponseWriter, r *http.Request) {
	var req RegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.WriteJSON(w, 400, map[string]string{"error": "invalid request body"})
		return
	}
	if len(req.Username) < 3 || len(req.Password) < 8 {
		httputil.WriteJSON(w, 400, map[string]string{"error": "username must be 3+ chars, password 8+ chars"})
		return
	}
	if len(req.Password) > maxPasswordLen {
		httputil.WriteJSON(w, 400, map[string]string{"error": "password must not exceed 72 characters"})
		return
	}
	if !strings.Contains(req.Email, "@") || len(req.Email) < 5 {
		httputil.WriteJSON(w, 400, map[string]string{"error": "a valid email address is required"})
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		httputil.WriteJSON(w, 500, map[string]string{"error": "internal error"})
		return
	}

	userID := uuid.New().String()
	_, err = h.DB.ExecContext(r.Context(),
		`INSERT INTO users (id, username, email, password_hash, display_name) VALUES (?, ?, ?, ?, ?)`,
		userID, req.Username, req.Email, string(hash), req.Username)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			httputil.WriteJSON(w, 409, map[string]string{"error": "username or email already taken"})
			return
		}
		httputil.WriteJSON(w, 500, map[string]string{"error": "failed to create user"})
		return
	}

	if _, err := h.DB.ExecContext(r.Context(), `INSERT INTO user_preferences (user_id) VALUES (?) ON CONFLICT DO NOTHING`, userID); err != nil {
		httputil.WriteJSON(w, 500, map[string]string{"error": "failed to initialize preferences"})
		return
	}

	token, err := h.generateToken(userID)
	if err != nil {
		httputil.WriteJSON(w, 500, map[string]string{"error": "failed to generate token"})
		return
	}

	httputil.WriteJSON(w, 201, map[string]string{"token": token, "user_id": userID})
}

// LoginRequest is the JSON body for POST /api/auth/login.
type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// HandleLogin authenticates an existing user.
func (h *Handler) HandleLogin(w http.ResponseWriter, r *http.Request) {
	var req LoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.WriteJSON(w, 400, map[string]string{"error": "invalid request body"})
		return
	}

	var userID, hash string
	err := h.DB.QueryRowContext(r.Context(),
		`SELECT id, password_hash FROM users WHERE username = ? OR email = ?`,
		req.Username, req.Username,
	).Scan(&userID, &hash)
	if err != nil {
		httputil.WriteJSON(w, 401, map[string]string{"error": "invalid credentials"})
		return
	}

	if len(req.Password) > maxPasswordLen {
		httputil.WriteJSON(w, 401, map[string]string{"error": "invalid credentials"})
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)); err != nil {
		httputil.WriteJSON(w, 401, map[string]string{"error": "invalid credentials"})
		return
	}
	token, err := h.generateToken(userID)
	if err != nil {
		httputil.WriteJSON(w, 500, map[string]string{"error": "failed to generate token"})
		return
	}
	httputil.WriteJSON(w, 200, map[string]string{"token": token, "user_id": userID})
}

func (h *Handler) generateToken(userID string) (string, error) {
	return GenerateToken(userID, h.JWTSecret), nil
}

// GenerateToken creates a signed JWT for the given user ID and secret.
func GenerateToken(userID, secret string) string {
	claims := jwt.MapClaims{
		"sub": userID,
		"exp": time.Now().Add(7 * 24 * time.Hour).Unix(),
		"iat": time.Now().Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	s, _ := token.SignedString([]byte(secret))
	return s
}

// ExtractUserIDFromToken parses the Bearer JWT from a request using the given secret.
func ExtractUserIDFromToken(r *http.Request, secret string) string {
	authHeader := r.Header.Get("Authorization")
	if !strings.HasPrefix(authHeader, "Bearer ") {
		return ""
	}

	tokenStr := strings.TrimPrefix(authHeader, "Bearer ")
	token, err := jwt.Parse(tokenStr, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method")
		}
		return []byte(secret), nil
	})

	if err != nil || !token.Valid {
		return ""
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return ""
	}

	sub, ok := claims["sub"].(string)
	if !ok {
		return ""
	}
	return sub
}

// AuthMiddleware requires a valid JWT and puts the user ID into the context.
func (h *Handler) AuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID := ExtractUserIDFromToken(r, h.JWTSecret)
		if userID == "" {
			httputil.WriteJSON(w, 401, map[string]string{"error": "unauthorized"})
			return
		}
		ctx := context.WithValue(r.Context(), UserIDKey, userID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// OptionalAuth injects the user ID into the context if a valid JWT is present,
// but does not reject unauthenticated requests.
func (h *Handler) OptionalAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := ExtractUserIDFromToken(r, h.JWTSecret)
		if userID != "" {
			ctx := context.WithValue(r.Context(), UserIDKey, userID)
			r = r.WithContext(ctx)
		}
		next(w, r)
	}
}
