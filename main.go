package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Repository struct {
	Name   string
	GitURL string
	WebURL string
	Path   string
}

type Config struct {
	Repositories     []Repository
	TelegramBotToken string
	TelegramChatID   string
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	
	cfg, err := getConfig()
	if err != nil {
		log.Fatalf("FATAL: Ошибка конфигурации: %v", err)
	}

	log.Printf("🚀 Скрипт отслеживания запущен. Репозиториев: %d", len(cfg.Repositories))

	var wg sync.WaitGroup
	for _, repo := range cfg.Repositories {
		wg.Add(1)
		go func(r Repository) {
			defer wg.Done()
			if err := checkRepository(r, cfg); err != nil {
				log.Printf("❌ ERROR: Ошибка при проверке [%s]: %v", r.Name, err)
			}
		}(repo)
	}
	wg.Wait()

	log.Println("✅ Проверка завершена.")
}

func getConfig() (Config, error) {
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	chatID := os.Getenv("TELEGRAM_CHAT_ID")
	
	log.Printf("🔑 Telegram Bot Token: %s", maskToken(token))
	log.Printf("💬 Telegram Chat ID: %s", chatID)
	
	if token == "" || chatID == "" {
		return Config{}, fmt.Errorf("переменные окружения TELEGRAM_BOT_TOKEN и TELEGRAM_CHAT_ID должны быть установлены")
	}

	return Config{
		Repositories: []Repository{
			{
				Name:   "nuclei-templates",
				GitURL: "https://github.com/projectdiscovery/nuclei-templates.git",
				WebURL: "https://github.com/projectdiscovery/nuclei-templates/blob/main",
				Path:   "nuclei-templates",
			},
			{
				Name:   "nucleihub-templates", 
				GitURL: "https://github.com/rix4uni/nucleihub-templates.git",
				WebURL: "https://github.com/rix4uni/nucleihub-templates/blob/main",
				Path:   "nucleihub-templates",
			},
		},
		TelegramBotToken: token,
		TelegramChatID:   chatID,
	}, nil
}

func maskToken(token string) string {
	if len(token) < 8 {
		return "***"
	}
	return token[:4] + "..." + token[len(token)-4:]
}

func checkRepository(repo Repository, cfg Config) error {
	log.Printf("🔍 [%s] Начинаю проверку репозитория", repo.Name)
	
	if err := prepareRepo(repo); err != nil {
		return fmt.Errorf("не удалось подготовить репозиторий: %w", err)
	}

	stateFile := fmt.Sprintf("known_templates_%s.txt", repo.Name)
	knownTemplates, err := readTemplatesFromFile(stateFile)
	isFirstRun := os.IsNotExist(err)

	if isFirstRun {
		log.Printf("🆕 [%s] Первый запуск, файл состояния не найден", repo.Name)
	} else {
		log.Printf("📄 [%s] Загружено %d известных шаблонов из файла состояния", repo.Name, len(knownTemplates))
	}

	currentTemplates, err := scanForTemplates(repo.Path)
	if err != nil {
		return fmt.Errorf("не удалось просканировать шаблоны: %w", err)
	}

	log.Printf("📊 [%s] Текущих шаблонов: %d", repo.Name, len(currentTemplates))

	var newTemplates []string
	for _, tpl := range currentTemplates {
		if _, found := knownTemplates[tpl]; !found {
			newTemplates = append(newTemplates, tpl)
		}
	}

	if isFirstRun {
		log.Printf("🔄 [%s] Первый запуск. Найдено %d шаблонов. Сохраняю состояние.", repo.Name, len(currentTemplates))
		message := fmt.Sprintf("✅ *Начинаю отслеживание репозитория `%s`*\\.\n\nОбнаружено и сохранено %d шаблонов\\. Уведомления будут приходить при появлении новых\\.", repo.Name, len(currentTemplates))
		if err := sendTelegramMessage(message, cfg.TelegramBotToken, cfg.TelegramChatID); err != nil {
			log.Printf("⚠️ WARN: Не удалось отправить стартовое уведомление для [%s]: %v", repo.Name, err)
		} else {
			log.Printf("📱 [%s] Стартовое уведомление отправлено", repo.Name)
		}
	} else if len(newTemplates) > 0 {
		log.Printf("🆕 [%s] Найдено %d новых шаблонов. Отправляю уведомление файлом...", repo.Name, len(newTemplates))
		
		// Формируем заголовок сообщения
		caption := fmt.Sprintf("🔔 *Обнаружены новые шаблоны в `%s` (%d шт\\.)*", repo.Name, len(newTemplates))

		// Создаем содержимое для файла
		var fileContent strings.Builder
		for _, tpl := range newTemplates {
			relativePath := strings.TrimPrefix(tpl, repo.Path+string(filepath.Separator))
			fileURL := fmt.Sprintf("%s/%s", repo.WebURL, relativePath)
			fileContent.WriteString(fmt.Sprintf("%s\n", fileURL))
		}
		
		// Отправляем как документ
		fileName := fmt.Sprintf("new_templates_%s_%s.txt", repo.Name, time.Now().Format("2006-01-02"))
		if err := sendTelegramFile(caption, fileName, fileContent.String(), cfg.TelegramBotToken, cfg.TelegramChatID); err != nil {
			log.Printf("❌ WARN: Не удалось отправить файл с уведомлением для [%s]: %v", repo.Name, err)
		} else {
			log.Printf("📱 [%s] Файл с новыми шаблонами отправлен", repo.Name)
		}
	} else {
		log.Printf("👍 [%s] Новых шаблонов не найдено.", repo.Name)
		// Отправляем краткий отчет, что проверка прошла
		message := fmt.Sprintf("🔍 *Проверка `%s` завершена*\\. Новых шаблонов нет\\.", repo.Name)
		if err := sendTelegramMessage(message, cfg.TelegramBotToken, cfg.TelegramChatID); err != nil {
			log.Printf("⚠️ WARN: Не удалось отправить отчет для [%s]: %v", repo.Name, err)
		}
	}

	if isFirstRun || len(newTemplates) > 0 {
		if err := writeTemplatesToFile(stateFile, currentTemplates); err != nil {
			return fmt.Errorf("не удалось сохранить состояние: %w", err)
		}
		log.Printf("💾 [%s] Состояние сохранено в %s", repo.Name, stateFile)
	}

	return nil
}

func prepareRepo(repo Repository) error {
	if _, err := os.Stat(repo.Path); os.IsNotExist(err) {
		log.Printf("📥 [%s] Клонирую репозиторий...", repo.Name)
		cmd := exec.Command("git", "clone", "--depth", "1", repo.GitURL, repo.Path)
		if output, err := cmd.CombinedOutput(); err != nil {
			log.Printf("❌ [%s] Ошибка клонирования: %s", repo.Name, string(output))
			return err
		}
	} else {
		log.Printf("🔄 [%s] Обновляю репозиторий...", repo.Name)
		cmd := exec.Command("git", "-C", repo.Path, "pull")
		if output, err := cmd.CombinedOutput(); err != nil {
			log.Printf("❌ [%s] Ошибка обновления: %s", repo.Name, string(output))
			return err
		}
	}
	return nil
}

func scanForTemplates(dir string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && (strings.HasSuffix(path, ".yaml") || strings.HasSuffix(path, ".yml")) {
			files = append(files, path)
		}
		return err
	})
	return files, err
}

func readTemplatesFromFile(file string) (map[string]bool, error) {
	f, err := os.Open(file)
	if err != nil {
		return make(map[string]bool), err
	}
	defer f.Close()
	templates := make(map[string]bool)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		templates[scanner.Text()] = true
	}
	return templates, scanner.Err()
}

func writeTemplatesToFile(file string, templates []string) error {
	log.Printf("💾 Запись %d шаблонов в файл %s", len(templates), file)
	f, err := os.Create(file)
	if err != nil {
		return err
	}
	defer f.Close()
	writer := bufio.NewWriter(f)
	for _, tpl := range templates {
		if _, err := writer.WriteString(tpl + "\n"); err != nil {
			return err
		}
	}
	return writer.Flush()
}

func sendTelegramMessage(message, token, chatID string) error {
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", token)
	payload, _ := json.Marshal(map[string]string{
		"chat_id":    chatID,
		"text":       message,
		"parse_mode": "MarkdownV2",
	})
	
	log.Printf("📤 Отправляю сообщение в Telegram...")
	resp, err := http.Post(apiURL, "application/json", bytes.NewBuffer(payload))
	if err != nil {
		return fmt.Errorf("ошибка HTTP запроса: %w", err)
	}
	defer resp.Body.Close()
	
	bodyBytes, _ := io.ReadAll(resp.Body)
	log.Printf("📥 Ответ Telegram API (status: %d): %s", resp.StatusCode, string(bodyBytes))
	
	if resp.StatusCode != http.StatusOK {
		var body map[string]interface{}
		json.Unmarshal(bodyBytes, &body)
		return fmt.Errorf("статус-код %d: %v", resp.StatusCode, body)
	}
	return nil
}

func sendTelegramFile(caption, fileName, fileContent, token, chatID string) error {
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendDocument", token)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	writer.WriteField("chat_id", chatID)
	writer.WriteField("caption", caption)
	writer.WriteField("parse_mode", "MarkdownV2")

	part, err := writer.CreateFormFile("document", fileName)
	if err != nil {
		return err
	}
	
	_, err = io.Copy(part, strings.NewReader(fileContent))
	if err != nil {
		return err
	}

	writer.Close()

	req, err := http.NewRequest("POST", apiURL, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	log.Printf("📤 Отправляю файл в Telegram...")
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("ошибка HTTP запроса: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	log.Printf("📥 Ответ Telegram API (status: %d): %s", resp.StatusCode, string(bodyBytes))

	if resp.StatusCode != http.StatusOK {
		var respBody map[string]interface{}
		json.Unmarshal(bodyBytes, &respBody)
		return fmt.Errorf("статус-код %d: %v", resp.StatusCode, respBody)
	}

	return nil
}
