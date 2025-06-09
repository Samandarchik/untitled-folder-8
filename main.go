package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"cloud.google.com/go/firestore"
	firebase "firebase.google.com/go/v4"
	"firebase.google.com/go/v4/auth"
	"google.golang.org/api/option"
)

type TelegramBotServer struct {
	BotToken          string
	BaseURL           string
	FirebaseProjectID string
	LastUpdateID      int
	IsRunning         bool
	FirebaseApp       *firebase.App
	AuthClient        *auth.Client
	FirestoreClient   *firestore.Client
}

type TelegramUpdate struct {
	UpdateID int              `json:"update_id"`
	Message  *TelegramMessage `json:"message,omitempty"`
}

type TelegramMessage struct {
	MessageID int              `json:"message_id"`
	From      *TelegramUser    `json:"from,omitempty"`
	Chat      *TelegramChat    `json:"chat,omitempty"`
	Text      string           `json:"text,omitempty"`
	Contact   *TelegramContact `json:"contact,omitempty"`
}

type TelegramUser struct {
	ID        int    `json:"id"`
	FirstName string `json:"first_name"`
	Username  string `json:"username,omitempty"`
}

type TelegramChat struct {
	ID int `json:"id"`
}

type TelegramContact struct {
	PhoneNumber string `json:"phone_number"`
	FirstName   string `json:"first_name"`
	UserID      int    `json:"user_id,omitempty"`
}

type TelegramResponse struct {
	OK     bool             `json:"ok"`
	Result []TelegramUpdate `json:"result"`
}

type UserInfo struct {
	PhoneNumber string    `json:"phone_number"`
	FirstName   string    `json:"first_name"`
	UserID      int       `json:"user_id"`
	CreatedAt   time.Time `json:"created_at"`
	Email       string    `json:"email"`
	Password    string    `json:"password"`
}

func NewTelegramBotServer() *TelegramBotServer {
	return &TelegramBotServer{
		BotToken:          "7609705273:AAGfEPZ2GYmd8ICgVjXXHGlwXiZWD3nYhP8",
		BaseURL:           "https://api.telegram.org/bot",
		FirebaseProjectID: "amur-restoran",
		LastUpdateID:      0,
		IsRunning:         false,
	}
}

func (bot *TelegramBotServer) log(message string) {
	timestamp := time.Now().Format("2006-01-02 15:04:05")
	fmt.Printf("[%s] %s\n", timestamp, message)
}

func (bot *TelegramBotServer) initFirebase() error {
	ctx := context.Background()

	// JSON fayldan Firebase Admin SDK ni ishga tushirish
	opt := option.WithCredentialsFile("amur-restoran-firebase-adminsdk-fbsvc-9d304ea78e.json")

	app, err := firebase.NewApp(ctx, &firebase.Config{
		ProjectID: bot.FirebaseProjectID,
	}, opt)
	if err != nil {
		return fmt.Errorf("Firebase app yaratishda xatolik: %v", err)
	}

	bot.FirebaseApp = app

	// Auth client
	authClient, err := app.Auth(ctx)
	if err != nil {
		return fmt.Errorf("Firebase Auth client yaratishda xatolik: %v", err)
	}
	bot.AuthClient = authClient

	// Firestore client
	firestoreClient, err := app.Firestore(ctx)
	if err != nil {
		return fmt.Errorf("Firestore client yaratishda xatolik: %v", err)
	}
	bot.FirestoreClient = firestoreClient

	bot.log("Firebase muvaffaqiyatli ishga tushdi")
	return nil
}

func (bot *TelegramBotServer) generatePassword(userID int) string {
	userIDStr := strconv.Itoa(userID)
	if len(userIDStr) >= 6 {
		return userIDStr[len(userIDStr)-6:]
	}
	return fmt.Sprintf("%06s", userIDStr)
}

func (bot *TelegramBotServer) saveUserToFirestore(phoneNumber, firstName string, userID int) error {
	ctx := context.Background()

	userInfo := UserInfo{
		PhoneNumber: phoneNumber,
		FirstName:   firstName,
		UserID:      userID,
		CreatedAt:   time.Now(),
		Email:       fmt.Sprintf("%s@gmail.com", phoneNumber),
		Password:    bot.generatePassword(userID),
	}

	docRef := bot.FirestoreClient.Collection("userInfo").Doc(strconv.Itoa(userID))
	_, err := docRef.Set(ctx, userInfo)
	if err != nil {
		return fmt.Errorf("Firestore ga saqlashda xatolik: %v", err)
	}

	bot.log(fmt.Sprintf("Firestore ga saqlandi: %s", phoneNumber))
	return nil
}

func (bot *TelegramBotServer) createFirebaseUser(phoneNumber string, userID int) error {
	ctx := context.Background()
	email := fmt.Sprintf("%s@gmail.com", phoneNumber)
	password := bot.generatePassword(userID)

	params := (&auth.UserToCreate{}).
		Email(email).
		Password(password).
		DisplayName(fmt.Sprintf("User_%d", userID))

	userRecord, err := bot.AuthClient.CreateUser(ctx, params)
	if err != nil {
		// Foydalanuvchi allaqachon mavjud bo'lsa
		if strings.Contains(err.Error(), "already exists") {
			bot.log(fmt.Sprintf("Foydalanuvchi allaqachon mavjud: %s", email))
			return nil
		}
		return fmt.Errorf("Firebase Auth da foydalanuvchi yaratishda xatolik: %v", err)
	}

	bot.log(fmt.Sprintf("Firebase Auth da yaratildi: %s (UID: %s)", email, userRecord.UID))
	return nil
}

func (bot *TelegramBotServer) startBot() {
	bot.IsRunning = true
	bot.log("Telegram Bot serveri ishga tushdi...")
	bot.log(fmt.Sprintf("Bot Token: %s...", bot.BotToken[:10]))

	for bot.IsRunning {
		if err := bot.getUpdates(); err != nil {
			bot.log(fmt.Sprintf("Bot xatoligi: %v", err))
			time.Sleep(5 * time.Second)
		} else {
			time.Sleep(2 * time.Second)
		}
	}
}

func (bot *TelegramBotServer) getUpdates() error {
	url := fmt.Sprintf("%s%s/getUpdates?offset=%d", bot.BaseURL, bot.BotToken, bot.LastUpdateID+1)
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("getUpdates xatoligi: Status %d", resp.StatusCode)
	}

	var telegramResp TelegramResponse
	if err := json.NewDecoder(resp.Body).Decode(&telegramResp); err != nil {
		return err
	}

	if telegramResp.OK && len(telegramResp.Result) > 0 {
		for _, update := range telegramResp.Result {
			bot.LastUpdateID = update.UpdateID
			bot.processUpdate(update)
		}
	}

	return nil
}

func (bot *TelegramBotServer) processUpdate(update TelegramUpdate) {
	if update.Message == nil {
		return
	}

	message := update.Message
	chatID := message.Chat.ID
	userID := message.From.ID
	firstName := message.From.FirstName
	username := message.From.Username

	bot.log(fmt.Sprintf("Yangi xabar: User ID %d, Name: %s", userID, firstName))

	// /start komandasi
	if message.Text == "/start" {
		bot.handleStartCommand(chatID, userID, firstName, username)
	} else if message.Contact != nil {
		// Kontakt yuborilgan
		bot.handleContact(message, chatID, userID)
	} else if message.Text != "" {
		// Boshqa xabarlar
		bot.log(fmt.Sprintf("Matn xabar: %s", message.Text))
		bot.sendMessage(chatID, fmt.Sprintf("Sizning xabaringiz qabul qilindi: %s", message.Text), nil)
	}
}

func (bot *TelegramBotServer) handleStartCommand(chatID, userID int, firstName, username string) {
	keyboard := map[string]interface{}{
		"keyboard": [][]map[string]interface{}{
			{
				{
					"text":            "📱 Telefon raqamini yuborish",
					"request_contact": true,
				},
			},
		},
		"resize_keyboard":   true,
		"one_time_keyboard": true,
	}

	welcomeMessage := fmt.Sprintf(`🤖 Assalomu alaykum, %s!

Amur Restoran botiga xush kelibsiz!

Ro'yxatdan o'tish uchun telefon raqamingizni yuboring.`, firstName)

	bot.sendMessage(chatID, welcomeMessage, keyboard)
	bot.log(fmt.Sprintf("Start komandasi yuborildi: %d", userID))
}
func (bot *TelegramBotServer) handleContact(message *TelegramMessage, chatID, userID int) {
	contact := message.Contact
	phoneNumber := contact.PhoneNumber
	firstName := contact.FirstName
	contactUserID := contact.UserID

	bot.log(fmt.Sprintf("Kontakt qabul qilindi: %s, User: %s", phoneNumber, firstName))

	// Ma'lumotlarni saqlash
	if err := bot.saveUserToFirestore(phoneNumber, firstName, contactUserID); err != nil {
		bot.log(fmt.Sprintf("saveUserToFirestore xatoligi: %v", err))
	}

	if err := bot.createFirebaseUser(phoneNumber, contactUserID); err != nil {
		bot.log(fmt.Sprintf("createFirebaseUser xatoligi: %v", err))
	}

	password := bot.generatePassword(contactUserID)
	// email := fmt.Sprintf("%s@gmail.com", phoneNumber) // Bu qatorni o'zgarishsiz qoldiramiz, chunki Firebase Auth uchun email kerak.

	successMessage := fmt.Sprintf(`✅ Ma'lumotlaringiz muvaffaqiyatli saqlandi!

📧 Login: %s
🔐 Parol: %s

Endi siz ilovaga kirish uchun ushbu ma'lumotlardan foydalanishingiz mumkin.`, phoneNumber, password) // O'zgarish shu yerda: 'email' o'rniga 'phoneNumber'

	bot.sendMessage(chatID, successMessage, nil)
	bot.log(fmt.Sprintf("Ma'lumot saqlandi: %s", phoneNumber))
}

func (bot *TelegramBotServer) sendMessage(chatID int, text string, replyMarkup map[string]interface{}) {
	data := map[string]interface{}{
		"chat_id":    chatID,
		"text":       text,
		"parse_mode": "HTML",
	}

	if replyMarkup != nil {
		data["reply_markup"] = replyMarkup
	}

	jsonData, err := json.Marshal(data)
	if err != nil {
		bot.log(fmt.Sprintf("JSON marshal xatoligi: %v", err))
		return
	}

	url := fmt.Sprintf("%s%s/sendMessage", bot.BaseURL, bot.BotToken)
	resp, err := http.Post(url, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		bot.log(fmt.Sprintf("sendMessage xatoligi: %v", err))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == 200 {
		bot.log(fmt.Sprintf("Xabar yuborildi: Chat ID %d", chatID))
	} else {
		body, _ := io.ReadAll(resp.Body)
		bot.log(fmt.Sprintf("Xabar yuborishda xatolik: %d - %s", resp.StatusCode, string(body)))
	}
}

func (bot *TelegramBotServer) stopBot() {
	bot.IsRunning = false
	bot.log("Bot to'xtatildi")
}

func (bot *TelegramBotServer) startWebServer() {
	http.HandleFunc("/status", bot.handleStatus)
	http.HandleFunc("/start", bot.handleStart)
	http.HandleFunc("/stop", bot.handleStop)

	bot.log("Web server ishga tushdi: http://localhost:8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		bot.log(fmt.Sprintf("Web server xatoligi: %v", err))
	}
}

func (bot *TelegramBotServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	status := map[string]interface{}{
		"bot_running":    bot.IsRunning,
		"last_update_id": bot.LastUpdateID,
		"timestamp":      time.Now().Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

func (bot *TelegramBotServer) handleStart(w http.ResponseWriter, r *http.Request) {
	if !bot.IsRunning {
		go bot.startBot()
	}
	w.Write([]byte("Bot started"))
}

func (bot *TelegramBotServer) handleStop(w http.ResponseWriter, r *http.Request) {
	bot.stopBot()
	w.Write([]byte("Bot stopped"))
}

func main() {
	bot := NewTelegramBotServer()

	// Firebase ni ishga tushirish
	if err := bot.initFirebase(); err != nil {
		log.Fatalf("Firebase ishga tushirishda xatolik: %v", err)
	}

	// Signal handling
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		fmt.Println("\nSignal qabul qilindi, dastur to'xtatilmoqda...")
		bot.stopBot()
		if bot.FirestoreClient != nil {
			bot.FirestoreClient.Close()
		}
		os.Exit(0)
	}()

	fmt.Println("=================================")
	fmt.Println("🤖 Telegram Bot Backend Server")
	fmt.Println("=================================")
	fmt.Println("Status: http://localhost:8080/status")
	fmt.Println("Start: http://localhost:8080/start")
	fmt.Println("Stop: http://localhost:8080/stop")
	fmt.Println("=================================")

	// Web server va botni parallel ravishda ishga tushirish
	go bot.startBot()
	bot.startWebServer()
}
