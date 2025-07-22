package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
 )

// Repository описывает отслеживаемый репозиторий
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
	cfg, err := getConfig()
	if err != nil {
		log.Fatalf("FATAL: Ошибка конфигурации: %v", err)
	}

	log.Println("Скрипт отслеживания запущен.")

	var wg sync.WaitGroup
	for _, repo := range cfg.Repositories {
		wg.Add(1)
		go func(r Repository) {
			defer wg.Done()
			if err := checkRepository(r, cfg); err != nil {
				log.Printf("ERROR: Ошибка при проверке [%s]: %v", r.Name, err)
			}
		}(repo)
	}
	wg.Wait()

	log.Println("Проверка завершена.")
}

func getConfig() (Config, error) {
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	chatID := os.Getenv("TELEGRAM_CHAT_ID")
	if token == "" || chatID == "" {
		return Config{}, fmt.Errorf("переменные окружения TELEGRAM_BOT_TOKEN и TELEGRAM_CHAT_ID должны быть установлены")
	}

	return Config{
		Repositories: []Repository{
			{
				Name:   "nuclei-templates",
				GitURL: "https://github.com/projectdiscovery/nuclei-templates.git",
				WebURL: "https://github.com/projectdiscovery/nuclei-templates/blob/master",
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

func checkRepository(repo Repository, cfg Config ) error {
	if err := prepareRepo(repo); err != nil {
		return fmt.Errorf("не удалось подготовить репозиторий: %w", err)
	}

	stateFile := fmt.Sprintf("known_templates_%s.txt", repo.Name)
	_, err := os.Stat(stateFile)
	isFirstRun := os.IsNotExist(err)

	knownTemplates, _ := readTemplatesFromFile(stateFile)
	currentTemplates, err := scanForTemplates(repo.Path)
	if err != nil {
		return fmt.Errorf("не удалось просканировать шаблоны: %w", err)
	}

	var newTemplates []string
	for _, tpl := range currentTemplates {
		if _, found := knownTemplates[tpl]; !found {
			newTemplates = append(newTemplates, tpl)
		}
	}

	if isFirstRun {
		log.Printf("[%s] Первый запуск. Найдено %d шаблонов. Сохраняю состояние.", repo.Name, len(currentTemplates))
		// ИЗМЕНЕНИЕ ЗДЕСЬ: Добавлен обратный слэш перед точками
		message := fmt.Sprintf("✅ *Начинаю отслеживание репозитория `%s`*\\.\n\nОбнаружено и сохранено %d шаблонов\\. Уведомления будут приходить при появлении новых\\.", repo.Name, len(currentTemplates))
		if err := sendTelegramMessage(message, cfg.TelegramBotToken, cfg.TelegramChatID); err != nil {
			log.Printf("WARN: Не удалось отправить стартовое уведомление для [%s]: %v", repo.Name, err)
		}
	} else if len(newTemplates) > 0 {
		log.Printf("[%s] Найдено %d новых шаблонов. Отправляю уведомление...", repo.Name, len(newTemplates))
		
		var msg strings.Builder
		// ИЗМЕНЕНИЕ ЗДЕСЬ: Добавлен обратный слэш перед точкой в "шт."
		msg.WriteString(fmt.Sprintf("🔔 *Обнаружены новые шаблоны в `%s` (%d шт\\.)*\n\n", repo.Name, len(newTemplates)))
		for _, tpl := range newTemplates {
			relativePath := strings.TrimPrefix(tpl, repo.Path+string(filepath.Separator))
			fileURL := fmt.Sprintf("%s/%s", repo.WebURL, relativePath)
			// В ссылках ничего экранировать не нужно
			msg.WriteString(fmt.Sprintf("• [%s](%s)\n", relativePath, fileURL))
		}

		if err := sendTelegramMessage(msg.String(), cfg.TelegramBotToken, cfg.TelegramChatID); err != nil {
			log.Printf("WARN: Не удалось отправить уведомление для [%s]: %v", repo.Name, err)
		}
	} else {
		log.Printf("[%s] Новых шаблонов не найдено.", repo.Name)
	}

	if isFirstRun || len(newTemplates) > 0 {
		return writeTemplatesToFile(stateFile, currentTemplates)
	}

	return nil
}

func prepareRepo(repo Repository) error {
	if _, err := os.Stat(repo.Path); os.IsNotExist(err) {
		log.Printf("[%s] Клонирую репозиторий...", repo.Name)
		return exec.Command("git", "clone", "--depth", "1", repo.GitURL, repo.Path).Run()
	}
	log.Printf("[%s] Обновляю репозиторий...", repo.Name)
	return exec.Command("git", "-C", repo.Path, "pull").Run()
}

func scanForTemplates(dir string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, ".yaml") {
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
	log.Printf("Запись %d шаблонов в файл %s", len(templates), file)
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

// Используем parse_mode = "MarkdownV2", он более строгий
func sendTelegramMessage(message string, token, chatID string) error {
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", token )
	payload, _ := json.Marshal(map[string]string{
		"chat_id":    chatID,
		"text":       message,
		"parse_mode": "MarkdownV2",
	})

	resp, err := http.Post(apiURL, "application/json", bytes.NewBuffer(payload ))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var body map[string]interface{}
		json.NewDecoder(resp.Body ).Decode(&body)
		return fmt.Errorf("неожиданный статус-код от Telegram API: %d, ответ: %v", resp.StatusCode, body)
	}
	return nil
}
