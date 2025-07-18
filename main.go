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

type Repository struct {
	Name, URL, Path string
}

type Config struct {
	Repositories     []Repository
	TelegramBotToken string
	TelegramChatID   string
	IsTestRun        bool
}

func main() {
	cfg, err := getConfig()
	if err != nil {
		log.Fatalf("FATAL: Ошибка конфигурации: %v", err)
	}

	log.Println("Скрипт отслеживания запущен.")
	if cfg.IsTestRun {
		log.Println("РЕЖИМ ТЕСТИРОВАНИЯ АКТИВИРОВАН.")
	}

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
			{"nuclei-templates", "https://github.com/projectdiscovery/nuclei-templates.git", "nuclei-templates"},
			{"nucleihub-templates", "https://github.com/rix4uni/nucleihub-templates.git", "nucleihub-templates"},
		},
		TelegramBotToken: token,
		TelegramChatID:   chatID,
		IsTestRun:        strings.ToLower(os.Getenv("FORCE_TEST_NOTIFICATION")) == "true",,
	}, nil
}

func checkRepository(repo Repository, cfg Config) error {
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

	// --- Основная логика отправки ---
	if cfg.IsTestRun {
		log.Printf("[%s] Отправка тестового уведомления...", repo.Name)
		testTemplates := []string{fmt.Sprintf("test/template-1-from-%s.yaml", repo.Name), "test/template-2.yaml"}
		return notifyAboutNewTemplates(repo, testTemplates, cfg.TelegramBotToken, cfg.TelegramChatID)
	}

	if len(newTemplates) > 0 {
		if isFirstRun {
			log.Printf("[%s] Первый запуск. Найдено %d шаблонов. Сохраняю состояние, уведомление не отправлено.", repo.Name, len(currentTemplates))
		} else {
			log.Printf("[%s] Найдено %d новых шаблонов. Отправляю уведомление...", repo.Name, len(newTemplates))
			if err := notifyAboutNewTemplates(repo, newTemplates, cfg.TelegramBotToken, cfg.TelegramChatID); err != nil {
				log.Printf("WARN: Не удалось отправить уведомление для [%s]: %v", repo.Name, err)
			}
		}
		return writeTemplatesToFile(stateFile, currentTemplates)
	}

	log.Printf("[%s] Новых шаблонов не найдено.", repo.Name)
	return nil
}

// ... (остальные функции без изменений)
func prepareRepo(repo Repository) error {
	if _, err := os.Stat(repo.Path); os.IsNotExist(err) {
		log.Printf("[%s] Клонирую репозиторий...", repo.Name)
		return exec.Command("git", "clone", "--depth", "1", repo.URL, repo.Path).Run()
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
