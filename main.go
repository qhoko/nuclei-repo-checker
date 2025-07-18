// Вставьте сюда этот безопасный код
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
	Name string // Уникальное имя для логов и файлов
	URL  string // Git URL
	Path string // Локальная папка
}

// Config хранит конфигурацию скрипта
type Config struct {
	Repositories     []Repository
	TelegramBotToken string
	TelegramChatID   string
}

func main() {
	cfg, err := getConfig()
	if err != nil {
		log.Fatalf("Ошибка конфигурации: %v", err)
	}

	log.Println("Скрипт отслеживания запущен.")

	var wg sync.WaitGroup
	for _, repo := range cfg.Repositories {
		wg.Add(1)
		go func(r Repository) {
			defer wg.Done()
			if err := checkRepository(r, cfg.TelegramBotToken, cfg.TelegramChatID); err != nil {
				log.Printf("Ошибка при проверке [%s]: %v", r.Name, err)
			}
		}(repo)
	}
	wg.Wait()

	log.Println("Проверка завершена.")
}

// getConfig собирает конфигурацию из переменных окружения
func getConfig() (Config, error) {
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	chatID := os.Getenv("TELEGRAM_CHAT_ID")

	if token == "" || chatID == "" {
		return Config{}, fmt.Errorf("переменные окружения TELEGRAM_BOT_TOKEN и TELEGRAM_CHAT_ID должны быть установлены")
	}

	return Config{
		Repositories: []Repository{
			{"nuclei-templates", "https://github.com/projectdiscovery/nuclei-templates.git", "nuclei-templates"},
			{"nucleihub-templates", "https://github.com/rix4uni/nucleihub-templates.git", "nucleihub-templates"},
		},
		TelegramBotToken: token,
		TelegramChatID:   chatID,
	}, nil
}

// checkRepository выполняет всю логику для одного репозитория
func checkRepository(repo Repository, token, chatID string ) error {
	// Шаг 1: Клонировать или обновить репозиторий
	if err := prepareRepo(repo); err != nil {
		return fmt.Errorf("не удалось подготовить репозиторий: %w", err)
	}

	stateFile := fmt.Sprintf("known_templates_%s.txt", repo.Name)
	_, err := os.Stat(stateFile)
	isFirstRun := os.IsNotExist(err)

	// Шаг 2: Получить списки шаблонов
	knownTemplates, _ := readTemplatesFromFile(stateFile)
	currentTemplates, err := scanForTemplates(repo.Path)
	if err != nil {
		return fmt.Errorf("не удалось просканировать шаблоны: %w", err)
	}

	// Шаг 3: Найти новые шаблоны
	var newTemplates []string
	for _, tpl := range currentTemplates {
		if _, found := knownTemplates[tpl]; !found {
			newTemplates = append(newTemplates, tpl)
		}
	}

	// Шаг 4: Отправить уведомление и/или обновить состояние
	if len(newTemplates) > 0 {
		if !isFirstRun {
			if err := notifyAboutNewTemplates(repo, newTemplates, token, chatID); err != nil {
				log.Printf("Предупреждение: не удалось отправить уведомление для [%s]: %v", repo.Name, err)
			}
		}
		if err := writeTemplatesToFile(stateFile, currentTemplates); err != nil {
			return fmt.Errorf("не удалось обновить файл состояния: %w", err)
		}
	}

	return nil
}

// --- Вспомогательные функции ---

func prepareRepo(repo Repository) error {
	if _, err := os.Stat(repo.Path); os.IsNotExist(err) {
		return exec.Command("git", "clone", "--depth", "1", repo.URL, repo.Path).Run()
	}
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

func notifyAboutNewTemplates(repo Repository, templates []string, token, chatID string) error {
	var msg strings.Builder
	msg.WriteString(fmt.Sprintf("🔔 *Обнаружены новые шаблоны в `%s` (%d шт.):*\n\n", repo.Name, len(templates)))
	for _, tpl := range templates {
		cleanPath := strings.TrimPrefix(tpl, repo.Path+string(filepath.Separator))
		msg.WriteString(fmt.Sprintf("`%s`\n", cleanPath))
	}

	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", token )
	payload, _ := json.Marshal(map[string]string{
		"chat_id":    chatID,
		"text":       msg.String(),
		"parse_mode": "Markdown",
	})

	resp, err := http.Post(apiURL, "application/json", bytes.NewBuffer(payload ))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("неожиданный статус-код от Telegram API: %d", resp.StatusCode )
	}

	log.Printf("Уведомление для [%s] успешно отправлено.", repo.Name)
	return nil
}