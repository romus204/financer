package tg

type Update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *Message       `json:"message"`
	CallbackQuery *CallbackQuery `json:"callback_query"`
}

type User struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
}

type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type Message struct {
	MessageID int    `json:"message_id"`
	From      *User  `json:"from"`
	Chat      Chat   `json:"chat"`
	Text      string `json:"text"`
}

type CallbackQuery struct {
	ID      string   `json:"id"`
	From    User     `json:"from"`
	Message *Message `json:"message"`
	Data    string   `json:"data"`
}

type InlineButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
}

type InlineKeyboard struct {
	InlineKeyboard [][]InlineButton `json:"inline_keyboard"`
}

// Add добавляет ряд кнопок; пустые ряды пропускаются.
func (k *InlineKeyboard) Add(row ...InlineButton) *InlineKeyboard {
	if len(row) > 0 {
		k.InlineKeyboard = append(k.InlineKeyboard, row)
	}
	return k
}

type KeyboardButton struct {
	Text string `json:"text"`
}

type ReplyKeyboard struct {
	Keyboard              [][]KeyboardButton `json:"keyboard"`
	ResizeKeyboard        bool               `json:"resize_keyboard"`
	IsPersistent          bool               `json:"is_persistent"`
	InputFieldPlaceholder string             `json:"input_field_placeholder,omitempty"`
}

type BotCommand struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}
