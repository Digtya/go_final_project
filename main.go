package main

import (
	"database/sql"
	"encoding/json"
	"io"

	//"errors"
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

func init() {
	log.SetOutput(os.Stdout)                     // Вывод логов в консоль
	log.SetFlags(log.LstdFlags | log.Lshortfile) // Добавляем время и файл с номером строки
}

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

	// Маршрут для статических файлов (если нужно)
	router.Get("/*", func(w http.ResponseWriter, r *http.Request) {
		filePath := filepath.Join(config.WebDir, strings.TrimPrefix(r.URL.Path, "/"))
		http.ServeFile(w, r, filePath)
	})

	// API маршруты
	router.Get("/api/nextdate", nextDateHandler)
	router.Route("/api/task", func(r chi.Router) {
		r.Post("/", withDB(createTaskHandler))       // POST для создания задачи
		r.Get("/", withDB(getTaskHandler))           // GET для получения задачи (с query-параметром id)
		r.Get("/{id}", withDB(getTaskHandler))       // GET для получения задачи (с параметром в URL)
		r.Delete("/{id}", withDB(deleteTaskHandler)) // DELETE для удаления задачи
		r.Put("/{id}", withDB(updateTaskHandler))    // PUT для обновления задачи
	})
	router.Get("/api/tasks", withDB(getTasksHandler))          // GET для получения списка задач
	router.Post("/api/task/done", withDB(markTaskDoneHandler)) // POST для отметки задачи как выполненной

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

	// Отладочный вывод
	//log.Printf("Received task: Date=%s, Title=%s, Comment=%s, Repeat=%s", task.Date, task.Title, task.Comment, task.Repeat)

	// Проверка на указание заголовка
	if task.Title == "" {
		writeError(w, http.StatusBadRequest, "Заголовок нужно обязательно указать")
		return
	}

	// Обработка даты
	today := time.Now()
	if task.Date == "" || task.Date == "today" || task.Date == today.Format("20060102") {
		// Если дата пустая, указана как "today" или равна текущей дате, устанавливаем текущую дату
		task.Date = today.Format("20060102")
	} else {
		// Парсим дату, если она указана
		parsedDate, err := time.Parse("20060102", task.Date)
		if err != nil {
			writeError(w, http.StatusBadRequest, "Неверный формат даты")
			return
		}

		// Если дата в прошлом, корректируем её
		if parsedDate.Before(today) {
			if task.Repeat == "" {
				// Если задача не повторяется, устанавливаем текущую дату
				task.Date = today.Format("20060102")
			} else {
				// Если задача повторяется, вычисляем следующую дату
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

	// Отладочный вывод
	//log.Printf("Task inserted: Date=%s, Title=%s, Comment=%s, Repeat=%s", task.Date, task.Title, task.Comment, task.Repeat)

	taskID, _ := result.LastInsertId()
	writeJSON(w, http.StatusOK, Response{ID: taskID})
}

// получение задачи
func getTaskHandler(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	// Настраиваем логгер для вывода в консоль
	log.SetOutput(os.Stdout)
	log.SetFlags(log.LstdFlags | log.Lshortfile) // Добавляем время и файл с номером строки

	log.Println("--- Функция getTaskHandler вызвана ---")

	// Получаем идентификатор задачи из query-параметра
	taskID := r.URL.Query().Get("id")
	if taskID == "" {
		log.Println("Ошибка: Не указан идентификатор задачи")
		writeError(w, http.StatusBadRequest, "Не указан идентификатор")
		return
	}

	// Преобразуем идентификатор в число
	id, err := strconv.ParseInt(taskID, 10, 64)
	if err != nil {
		log.Printf("Ошибка преобразования ID: %v\n", err)
		writeError(w, http.StatusBadRequest, "Неверный формат идентификатора")
		return
	}

	// Получаем задачу из базы данных
	var task Task
	query := `SELECT id, date, title, comment, repeat FROM scheduler WHERE id = ?`
	err = db.QueryRow(query, id).Scan(&task.ID, &task.Date, &task.Title, &task.Comment, &task.Repeat)
	if err != nil {
		if err == sql.ErrNoRows {
			log.Println("Ошибка: Задача не найдена")
			writeError(w, http.StatusNotFound, "Задача не найдена")
		} else {
			log.Printf("Ошибка выполнения SQL-запроса: %v\n", err)
			writeError(w, http.StatusInternalServerError, "Ошибка при получении задачи")
		}
		return
	}

	// Логируем полученные данные
	log.Printf("Получена задача: ID=%d, Date=%s, Title=%s, Comment=%s, Repeat=%s\n", task.ID, task.Date, task.Title, task.Comment, task.Repeat)

	// Возвращаем задачу в формате JSON
	writeJSON(w, http.StatusOK, map[string]string{
		"id":      strconv.FormatInt(task.ID, 10),
		"date":    task.Date,
		"title":   task.Title,
		"comment": task.Comment,
		"repeat":  task.Repeat,
	})
}

// обновление задачи
func updateTaskHandler(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	// Логирование начала выполнения функции
	log.Println("--- Функция updateTaskHandler вызвана ---")

	// Чтение тела запроса
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("Ошибка чтения тела запроса: %v\n", err)
		writeError(w, http.StatusBadRequest, "Ошибка чтения тела запроса")
		return
	}
	defer r.Body.Close()

	log.Printf("Тело запроса (после чтения): %s\n", string(body))

	// Проверка на пустое тело запроса
	if len(body) == 0 {
		log.Println("Тело запроса пустое")
		writeError(w, http.StatusBadRequest, "Тело запроса не может быть пустым")
		return
	}

	// Декодирование JSON
	var input struct {
		ID      int64  `json:"id"`
		Date    string `json:"date"`
		Title   string `json:"title"`
		Comment string `json:"comment"`
		Repeat  string `json:"repeat"`
	}

	if err := json.Unmarshal(body, &input); err != nil {
		log.Printf("Ошибка декодирования JSON: %v\n", err)
		writeError(w, http.StatusBadRequest, "Неверный формат JSON")
		return
	}

	log.Printf("Декодированные данные: ID=%d, Date=%s, Title=%s, Comment=%s, Repeat=%s\n", input.ID, input.Date, input.Title, input.Comment, input.Repeat)

	// Валидация ID задачи
	if input.ID == 0 {
		log.Println("Ошибка: ID задачи не указан")
		writeError(w, http.StatusBadRequest, "ID задачи не указан")
		return
	}

	// Валидация заголовка задачи
	if input.Title == "" {
		log.Println("Ошибка: Заголовок задачи не указан")
		writeError(w, http.StatusBadRequest, "Заголовок нужно обязательно указать")
		return
	}

	// Валидация даты
	today := time.Now()
	if input.Date == "" || input.Date == "today" {
		input.Date = today.Format("20060102")
	} else {
		parsedDate, err := time.Parse("20060102", input.Date)
		if err != nil {
			log.Printf("Ошибка: Неверный формат даты: %v\n", err)
			writeError(w, http.StatusBadRequest, "Неверный формат даты")
			return
		}

		// Если дата в прошлом, корректируем её
		if parsedDate.Before(today) {
			if input.Repeat == "" {
				input.Date = today.Format("20060102")
			} else {
				nextDate, err := nextDate(today, input.Date, input.Repeat)
				if err != nil {
					log.Printf("Ошибка: Не удалось вычислить следующую дату: %v\n", err)
					writeError(w, http.StatusBadRequest, err.Error())
					return
				}
				input.Date = nextDate
			}
		}
	}

	// Валидация повторения (если указано)
	if input.Repeat != "" {
		if !strings.HasPrefix(input.Repeat, "d ") && !strings.HasPrefix(input.Repeat, "y ") {
			log.Printf("Ошибка: Неверный формат повторения: %s\n", input.Repeat)
			writeError(w, http.StatusBadRequest, "Неверный формат повторения")
			return
		}
	}

	// Обновление задачи в базе данных
	query := `UPDATE scheduler SET date = ?, title = ?, comment = ?, repeat = ? WHERE id = ?`
	log.Printf("Выполнение SQL-запроса: %s с параметрами: Date=%s, Title=%s, Comment=%s, Repeat=%s, ID=%d\n", query, input.Date, input.Title, input.Comment, input.Repeat, input.ID)

	result, err := db.Exec(query, input.Date, input.Title, input.Comment, input.Repeat, input.ID)
	if err != nil {
		log.Printf("Ошибка выполнения SQL-запроса: %v\n", err)
		writeError(w, http.StatusInternalServerError, "Ошибка при обновлении задачи")
		return
	}

	// Проверка, была ли обновлена хотя бы одна строка
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		log.Printf("Ошибка при проверке обновления задачи: %v\n", err)
		writeError(w, http.StatusInternalServerError, "Ошибка при проверке обновления задачи")
		return
	}
	if rowsAffected == 0 {
		log.Println("Ошибка: Задача не найдена")
		writeError(w, http.StatusNotFound, "Задача не найдена")
		return
	}

	// Возвращаем успешный ответ с обновлёнными данными задачи
	updatedTask := Task{
		ID:      input.ID,
		Date:    input.Date,
		Title:   input.Title,
		Comment: input.Comment,
		Repeat:  input.Repeat,
	}

	log.Printf("Возвращаемый JSON: %+v\n", updatedTask)
	writeJSON(w, http.StatusOK, updatedTask)
}

// удаление задачи
func deleteTaskHandler(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "id")

	// Валидация ID задачи
	if _, err := strconv.Atoi(taskID); err != nil {
		writeError(w, http.StatusBadRequest, "ID задачи должен быть числом")
		return
	}

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

	// Валидация ID задачи
	if _, err := strconv.Atoi(id); err != nil {
		writeError(w, http.StatusBadRequest, "ID задачи должен быть числом")
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
	// Задаём лимит константой
	const limit = 10

	// Запрос задач из базы данных с использованием LIMIT
	query := `SELECT id, date, title, comment, repeat FROM scheduler ORDER BY date LIMIT ?`
	rows, err := db.Query(query, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Ошибка чтения задач")
		return
	}
	defer rows.Close()

	tasks := make([]map[string]string, 0) // Инициализируем пустой массив задач
	for rows.Next() {
		var (
			id      int64
			date    string
			title   string
			comment string
			repeat  string
		)
		if err := rows.Scan(&id, &date, &title, &comment, &repeat); err != nil {
			writeError(w, http.StatusInternalServerError, "Ошибка чтения задач")
			return
		}

		// Преобразуем задачу в map[string]string
		task := map[string]string{
			"id":      strconv.FormatInt(id, 10), // Преобразуем id в строку
			"date":    date,
			"title":   title,
			"comment": comment,
			"repeat":  repeat,
		}
		tasks = append(tasks, task)
	}

	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "Ошибка чтения задач")
		return
	}

	// Возвращаем список задач в формате, ожидаемом тестом
	writeJSON(w, http.StatusOK, map[string][]map[string]string{"tasks": tasks})
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
