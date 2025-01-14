package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	_ "github.com/mattn/go-sqlite3"
)

type Task struct {
	ID       int64  `json:"id"`
	Date     string `json:"date"`
	Title    string `json:"title"`
	Comment  string `json:"comment"`
	Repeat   string `json:"repeat"`
	NextDate string `json:"-"`
}

type Response struct {
	ID       int64  `json:"id,omitempty"`
	NextDate string `json:"next_date,omitempty"`
	Tasks    []Task `json:"tasks,omitempty"`
	Error    string `json:"error,omitempty"`
}

type Config struct {
	WebDir string
	Port   string
	DBPath string
}

const (
	dbName     = "scheduler.db"
	dateFormat = "2006-01-02"
	tableSQL   = `CREATE TABLE IF NOT EXISTS scheduler (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		date TEXT,
		title TEXT,
		comment TEXT,
		repeat TEXT CHECK(LENGTH(repeat) <= 128)
	);
	CREATE INDEX IF NOT EXISTS idx_date ON scheduler(date);`
)

var config Config

func main() {
	// закрепляем путь к БД, фронту и порт
	config = Config{
		WebDir: "./web",
		Port:   ":7540",
		DBPath: "./scheduler.db",
	}

	// Подключаем БД или создаём её
	appPath, err := os.Executable()
	if err != nil {
		log.Fatalf("Не удалось получить путь приложения: %v\n", err)
	}
	dbFile := filepath.Join(filepath.Dir(appPath), dbName)
	db, err := openDB(dbFile)
	if err != nil {
		log.Fatalf("Ошибка открытия базы данных: %v\n", err)
	}
	defer db.Close()

	if !tableExists(db, "scheduler") {
		createTable(db)
	}

	// Настройка маршрутов и запуск сервера
	router := setupRouter()
	log.Printf("Запуск сервера на порту %s...\n", config.Port)
	log.Fatal(http.ListenAndServe(config.Port, router))
}

func setupRouter() *chi.Mux {
	router := chi.NewRouter()

	router.Get("/*", func(w http.ResponseWriter, r *http.Request) {
		filePath := filepath.Join(config.WebDir, strings.TrimPrefix(r.URL.Path, "/"))
		http.ServeFile(w, r, filePath)
	})

	// API маршруты
	router.Get("/api/nextdate", nextDateHandler)
	router.Route("/api/task", func(r chi.Router) {
		r.With(PostRequestValidation).Post("/", withDB(createTaskHandler))
		r.With(GetRequestValidation).Get("/{id}", withDB(getTaskHandler))
		r.With(DeleteRequestValidation).Delete("/{id}", withDB(deleteTaskHandler))
		r.With(PutRequestValidation).Put("/{id}", withDB(updateTaskHandler))
	})
	router.Get("/api/tasks", withDB(getTasksHandler))
	router.Post("/api/task/done", withDB(markTaskDoneHandler))

	return router
}

// ------------------------ База данных ------------------------

func openDB(file string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", file)
	if err != nil {
		return nil, fmt.Errorf("не удалось открыть базу данных: %w", err)
	}
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("не удалось подключиться к базе данных: %w", err)
	}
	return db, nil
}

func tableExists(db *sql.DB, tableName string) bool {
	row := db.QueryRow(fmt.Sprintf(`SELECT name FROM sqlite_master WHERE type='table' AND name='%s';`, tableName))
	var name string
	err := row.Scan(&name)
	return err == nil && strings.EqualFold(name, tableName)
}

func createTable(db *sql.DB) {
	_, err := db.Exec(tableSQL)
	if err != nil {
		log.Fatalf("Ошибка создания таблицы: %v\n", err)
	}
	log.Println("Таблица 'scheduler' создана.")
}

func withDB(handler func(db *sql.DB, w http.ResponseWriter, r *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		db, err := openDB(config.DBPath)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "Database error")
			return
		}
		defer db.Close()
		handler(db, w, r)
	}
}

// ------------------------ Вспомогательные функции ------------------------

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, status int, errMsg string) {
	writeJSON(w, status, map[string]string{"error": errMsg})
}

// ------------------------ Обработчики ------------------------

// создание задачи
func createTaskHandler(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")

	var task Task
	if err := json.NewDecoder(r.Body).Decode(&task); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid input")
		return
	}

	// Проверка на указание заголовка
	if task.Title == "" {
		writeError(w, http.StatusBadRequest, "Заголовок нужно обязательно указать")
		return
	}

	// Проверяем дату
	today := time.Now()
	if task.Date == "" {
		task.Date = today.Format("20060102")
	} else {
		parsedDate, err := time.Parse("20060102", task.Date)
		if err != nil {
			writeError(w, http.StatusBadRequest, "Неверный формат даты")
			return
		}

		if parsedDate.Before(today) {
			if task.Repeat == "" {
				task.Date = today.Format("20060102")
			} else {
				nextDate, err := nextDate(today, task.Date, task.Repeat)
				if err != nil {
					writeError(w, http.StatusBadRequest, err.Error())
					return
				}
				task.Date = nextDate
			}
		}
	}

	// Вносим задачу в БД
	query := `INSERT INTO scheduler (date, title, comment, repeat) VALUES (?, ?, ?, ?)`
	result, err := db.Exec(query, task.Date, task.Title, task.Comment, task.Repeat)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Ошибка при создании задачи")
		return
	}

	taskID, _ := result.LastInsertId()
	writeJSON(w, http.StatusOK, Response{ID: taskID})
}

// получение задачи
func getTaskHandler(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "id")

	query := `SELECT id, date, title, comment, repeat FROM scheduler WHERE id = ?`
	row := db.QueryRow(query, taskID)

	var task Task
	err := row.Scan(&task.ID, &task.Date, &task.Title, &task.Comment, &task.Repeat)
	if err != nil {
		writeError(w, http.StatusNotFound, "Задача не найдена")
		return
	}

	writeJSON(w, http.StatusOK, task)
}

// обновление задачи
func updateTaskHandler(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "id")
	var task Task
	if err := json.NewDecoder(r.Body).Decode(&task); err != nil {
		writeError(w, http.StatusBadRequest, "Неверный ввод")
		return
	}

	query := `UPDATE scheduler SET date = ?, title = ?, comment = ?, repeat = ? WHERE id = ?`
	_, err := db.Exec(query, task.Date, task.Title, task.Comment, task.Repeat, taskID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Не удалось обновить задачу")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// удаление задачи
func deleteTaskHandler(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "id")

	query := `DELETE FROM scheduler WHERE id = ?`
	_, err := db.Exec(query, taskID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Не удалось удалить задачу")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// пометить задачу как выполненную
func markTaskDoneHandler(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	// Получение идентификатора задачи из параметров запроса
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, `{"error":"Идентификатор задачи не указан"}`, http.StatusBadRequest)
		return
	}

	// Получение задачи из базы данных
	var task Task
	err := db.QueryRow("SELECT * FROM scheduler WHERE id = ?", id).Scan(&task)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"Задача с id %s не найдена"}`, id), http.StatusNotFound)
		return
	}

	if task.Repeat == "" {
		// Если это одноразовая задача (поле repeat пустое), удаляем её
		_, err := db.Exec("DELETE FROM scheduler WHERE id = ?", id)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"Ошибка удаления задачи: %v"}`, err), http.StatusInternalServerError)
			return
		}

		// Возвращаем пустой JSON в случае успеха
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
		return
	}

	// Для периодической задачи рассчитываем следующую дату
	nextDate, err := nextDate(time.Now(), task.Date, task.Repeat)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"Ошибка расчёта следующей даты: %v"}`, err), http.StatusBadRequest)
		return
	}

	// Обновляем дату задачи в базе данных
	_, err = db.Exec("UPDATE scheduler SET date = ? WHERE id = ?", nextDate, id)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"Ошибка обновления задачи: %v"}`, err), http.StatusInternalServerError)
		return
	}

	// Возвращаем пустой JSON в случае успеха
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{}`))
}

// получение всех задач
func getTasksHandler(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	// Чтение параметров limit и offset из запроса
	limitStr := r.URL.Query().Get("limit")
	offsetStr := r.URL.Query().Get("offset")

	// Парсинг параметров с ограничениями
	limit := 10
	offset := 0
	if limitStr != "" {
		parsedLimit, err := strconv.Atoi(limitStr)
		if err != nil || parsedLimit < 10 || parsedLimit > 50 {
			writeError(w, http.StatusBadRequest, "Параметр limit должен быть числом от 10 до 50")
			return
		}
		limit = parsedLimit
	}
	if offsetStr != "" {
		parsedOffset, err := strconv.Atoi(offsetStr)
		if err != nil || parsedOffset < 0 {
			writeError(w, http.StatusBadRequest, "Параметр offset должен быть положительным числом")
			return
		}
		offset = parsedOffset
	}

	// Запрос задач из базы данных с использованием LIMIT и OFFSET
	query := `SELECT id, date, title, comment, repeat, done FROM scheduler ORDER BY date LIMIT ? OFFSET ?`
	rows, err := db.Query(query, limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Ошибка чтения задач")
		return
	}
	defer rows.Close()

	var tasks []Task
	for rows.Next() {
		var task Task
		if err := rows.Scan(&task.ID, &task.Date, &task.Title, &task.Comment, &task.Repeat); err != nil {
			writeError(w, http.StatusInternalServerError, "Ошибка чтения задач")
			return
		}
		tasks = append(tasks, task)
	}

	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "Ошибка чтения задач")
		return
	}

	// Возвращаем список задач (может быть пустым)
	writeJSON(w, http.StatusOK, Response{Tasks: tasks})
}

// функция nextDate вычисляет следующую дату выполнения задачи

func nextDate(now time.Time, dateStr, repeat string) (string, error) {
	// Проверка на пустую строку в repeat
	if repeat == "" {
		return "", fmt.Errorf("необходимо указать интервал повторения (repeat)")
	}

	// Парсинг исходной даты
	date, err := time.Parse("20060102", dateStr)
	if err != nil {
		return "", fmt.Errorf("неверный формат даты, не удалось преобразовать в корректную дату")
	}

	// Используем now, если он пустой
	if now.IsZero() {
		now = time.Now()
	}

	// Разделяем строку повторения на части
	parts := strings.Split(repeat, " ")
	if len(parts) < 1 || len(parts) > 2 {
		return "", fmt.Errorf("неверный формат repeat, требуется указать интервал и (при необходимости) количество")
	}

	interval := parts[0]
	var next time.Time

	switch interval {
	case "y": // Добавление года
		// Добавляем 1 год и проверяем, чтобы дата была не меньше текущей
		next = date.AddDate(1, 0, 0)
		for next.Before(now) {
			next = next.AddDate(1, 0, 0)
		}

	case "d": // Добавление дней
		if len(parts) > 1 {
			// Если интервал в днях не указан или некорректен, вернем ошибку
			days, err := strconv.Atoi(parts[1])
			if err != nil || days <= 0 || days > 400 {
				return "", fmt.Errorf("некорректный аргумент для 'd': %v, должно быть числом от 1 до 400", parts[1])
			}
			// Добавляем дни и проверяем, чтобы дата была не меньше текущей
			next = date.AddDate(0, 0, days)
			for next.Before(now) {
				next = next.AddDate(0, 0, days)
			}
		} else {
			return "", fmt.Errorf("не указан интервал для дней после 'd'")
		}

	default:
		return "", fmt.Errorf("неподдерживаемый интервал: %s", interval)
	}

	// Проверка на пустоту: если next не установлена
	if next.IsZero() {
		return "", fmt.Errorf("не удалось вычислить следующую дату")
	}

	// Возврат результата в формате "YYYYMMDD"
	return next.Format("20060102"), nil
}

// обработчик вычисления следующей даты
func nextDateHandler(w http.ResponseWriter, r *http.Request) {
	dateStr := r.URL.Query().Get("date")
	repeat := r.URL.Query().Get("repeat")
	nowStr := r.URL.Query().Get("now")

	if dateStr == "" || repeat == "" || nowStr == "" {
		http.Error(w, "Параметры 'date', 'repeat' и 'now' обязательны", http.StatusBadRequest)
		return
	}

	now, err := time.Parse("20060102", nowStr)
	if err != nil {
		http.Error(w, "Неверный формат 'now'", http.StatusBadRequest)
		return
	}

	next, err := nextDate(now, dateStr, repeat)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Устанавливаем Content-Type как текст и возвращаем только дату
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(next))
}

// функции для валидации запросов

// Validate проверяет корректность данных задачи
func (t *Task) Validate() error {
	// Если дата пустая, то устанавливаем её как текущую
	if t.Date == "" {
		t.Date = time.Now().Format("20060102")
	}

	// Проверка на корректность формата даты
	_, err := time.Parse("20060102", t.Date)
	if err != nil {
		return fmt.Errorf("неверный формат даты")
	}

	// Check if date is earlier than today
	today := time.Now()
	taskDate, _ := time.Parse("20060102", t.Date)
	if taskDate.Before(today) {
		return fmt.Errorf("дата не может быть меньше сегодняшней")
	}

	// If title is empty, return an error
	if t.Title == "" {
		return fmt.Errorf("заголовок задачи не может быть пустым")
	}

	// If repeat is invalid, return an error
	if t.Repeat != "" && !strings.Contains("wdy", t.Repeat) {
		return fmt.Errorf("неверный формат повторения")
	}

	return nil
}

// ValidateID проверяет корректность ID задачи
func (t *Task) ValidateID(id string) error {
	if _, err := strconv.Atoi(id); err != nil {
		return errors.New("ID задачи должен быть числом")
	}
	return nil
}

// PostRequestValidation проверяет данные задачи
func PostRequestValidation(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var task Task
		if err := json.NewDecoder(r.Body).Decode(&task); err != nil {
			writeError(w, http.StatusBadRequest, "Неверный ввод")
			return
		}

		// Валидация данных задачи
		if err := task.Validate(); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}

		next.ServeHTTP(w, r)
	})
}

// PutRequestValidation проверяет данные задачи
func PutRequestValidation(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var task Task
		if err := json.NewDecoder(r.Body).Decode(&task); err != nil {
			writeError(w, http.StatusBadRequest, "Неверный ввод")
			return
		}

		// Валидация данных задачи
		if err := task.Validate(); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}

		next.ServeHTTP(w, r)
	})
}

func GetRequestValidation(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		var task Task
		if err := task.ValidateID(id); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		next.ServeHTTP(w, r)
	})
}

func DeleteRequestValidation(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		var task Task
		if err := task.ValidateID(id); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		next.ServeHTTP(w, r)
	})
}
