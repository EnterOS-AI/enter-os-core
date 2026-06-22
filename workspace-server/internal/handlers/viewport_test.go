package handlers

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// ==================== GET /canvas/viewport ====================

func TestViewportGet_Success(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewViewportHandler()

	mock.ExpectQuery("SELECT x, y, zoom FROM canvas_viewport").
		WillReturnRows(sqlmock.NewRows([]string{"x", "y", "zoom"}).
			AddRow(100.5, 200.3, 1.5))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/canvas/viewport", nil)

	handler.Get(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["x"].(float64) != 100.5 {
		t.Errorf("expected x=100.5, got %v", resp["x"])
	}
	if resp["y"].(float64) != 200.3 {
		t.Errorf("expected y=200.3, got %v", resp["y"])
	}
	if resp["zoom"].(float64) != 1.5 {
		t.Errorf("expected zoom=1.5, got %v", resp["zoom"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestViewportGet_NoSavedViewport(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewViewportHandler()

	mock.ExpectQuery("SELECT x, y, zoom FROM canvas_viewport").
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/canvas/viewport", nil)

	handler.Get(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Should return defaults
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["x"].(float64) != 0 {
		t.Errorf("expected default x=0, got %v", resp["x"])
	}
	if resp["y"].(float64) != 0 {
		t.Errorf("expected default y=0, got %v", resp["y"])
	}
	if resp["zoom"].(float64) != 1 {
		t.Errorf("expected default zoom=1, got %v", resp["zoom"])
	}
}

// ==================== PUT /canvas/viewport ====================

func TestViewportSave_Success(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewViewportHandler()

	mock.ExpectExec("INSERT INTO canvas_viewport").
		WithArgs(150.0, 250.0, 2.0).
		WillReturnResult(sqlmock.NewResult(0, 1))

	body := `{"x":150,"y":250,"zoom":2}`
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("PUT", "/canvas/viewport", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Save(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "saved" {
		t.Errorf("expected status 'saved', got %v", resp["status"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestViewportSave_InvalidBody(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewViewportHandler()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("PUT", "/canvas/viewport", bytes.NewBufferString("not json"))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Save(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestViewportSave_DBError(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewViewportHandler()

	mock.ExpectExec("INSERT INTO canvas_viewport").
		WillReturnError(sql.ErrConnDone)

	body := `{"x":0,"y":0,"zoom":1}`
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("PUT", "/canvas/viewport", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Save(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
}
