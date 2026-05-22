package core


// ------------------------- 常量 -------------------------

const CardActionLayoutEqualColumns CardActionLayout = "equal_columns"

const (
	CardActionLayoutRow CardActionLayout = "row"
)

// ------------------------- 类 -------------------------

// 表示富文本结构,可以被具体的平台卡片渲染(Feishu Interactive Card, Telegram message, etc.)
// 或者降级到plain 文本
type Card struct {
	Header   *CardHeader
	Elements []CardElement
}

// 带有颜色的卡片头
type CardHeader struct {
	Title string
	Color string // blue, green, red, orange, purple, grey, turquoise, violet, indigo, wathet, yellow, carmine
}

// 接口适用于所有卡片内容
type CardElement interface {
	cardElement()
}

// 表示 CardActions element 内部的 clickable button
type CardButton struct {
	Text  string            // 展示label
	Type  string            // "primary", "default", "danger"
	Value string            // callback data, e.g. "cmd:/new", "cmd:/switch 3"
	Extra map[string]string // 额外 key-value 对 carried in the callback (平台特定)
}

// 渲染一行button
type CardActions struct {
	Buttons []CardButton
	Layout  CardActionLayout
}

// 控制CardActions行如何被平台渲染富格式能力
type CardActionLayout string

// CardNote 在底部呈现小脚注文本。
// Tag 是一个可选的机器可读标识符（不显示），供平台渲染器以编程方式识别和处理特定notes。
type CardNote struct {
	Text string
	Tag  string
}

// CardListItem 渲染一行，左侧显示描述文本，右侧显示一个按钮。
// 在飞书（Feishu）上，这映射到div+extra；在其他平台上，它会降级为文本行。
type CardListItem struct {
	Text     string            // left-side description
	BtnText  string            // button label
	BtnType  string            // "primary", "default", "danger"
	BtnValue string            // callback data
	Extra    map[string]string // additional key-value pairs carried in the callback
}

// CardMarkdown renders markdown-formatted text.
type CardMarkdown struct{ Content string }

// CardDivider renders a horizontal rule.
type CardDivider struct{}

// renders a dropdown selector.
// Feishu 映射为 select_static;其他平台纯文本.
type CardSelect struct {
	Placeholder string
	Options     []CardSelectOption
	InitValue   string // pre-selected option value (empty = none)
}

// item in a CardSelect dropdown.
type CardSelectOption struct {
	Text  string
	Value string
}

// ------------------------- 方法 -------------------------

func (CardMarkdown) cardElement() {}
func (CardDivider) cardElement()  {}
func (CardActions) cardElement()  {}
func (CardNote) cardElement()     {}
func (CardListItem) cardElement() {}
func (CardSelect) cardElement()   {}

// --- Builder API ---

// 提供了fluent API 用于构建Card 结构
type CardBuilder struct {
	card Card
}

// NewCard returns a new CardBuilder.
func NewCard() *CardBuilder {
	return &CardBuilder{}
}

// 添加一个水平divider
func (b *CardBuilder) Divider() *CardBuilder {
	b.card.Elements = append(b.card.Elements, CardDivider{})
	return b
}

// 用title和颜色设置card头
func (b *CardBuilder) Title(title, color string) *CardBuilder {
	b.card.Header = &CardHeader{Title: title, Color: color}
	return b
}

// 给card添加一个markdown元素
func (b *CardBuilder) Markdown(content string) *CardBuilder {
	if content != "" {
		b.card.Elements = append(b.card.Elements, CardMarkdown{Content: content})
	}
	return b
}

// 返回 构建的 Card.
func (b *CardBuilder) Build() *Card {
	c := b.card
	return &c
}

// 添加一个action行, 每个button应该再平台上和其宽度一致, 支持richer 布局
func (b *CardBuilder) ButtonsEqual(buttons ...CardButton) *CardBuilder {
	if len(buttons) > 0 {
		b.card.Elements = append(b.card.Elements, CardActions{Buttons: buttons, Layout: CardActionLayoutEqualColumns})
	}
	return b
}

// 添加一个list行
func (b *CardBuilder) ListItem(desc, btnText, btnValue string) *CardBuilder {
	b.card.Elements = append(b.card.Elements, CardListItem{
		Text: desc, BtnText: btnText, BtnType: "default", BtnValue: btnValue,
	})
	return b
}

// 类似ListItem 但允许指定按钮类型
func (b *CardBuilder) ListItemBtn(desc, btnText, BtnType, btnValue string) *CardBuilder {
	b.card.Elements = append(b.card.Elements, CardListItem{
		Text: desc, BtnText: btnText, BtnType: BtnType, BtnValue: btnValue,
	})
	return b
}

// 添加一个footnote元素
func (b *CardBuilder) Note(text string) *CardBuilder {
	if text != "" {
		b.card.Elements = append(b.card.Elements, CardNote{Text: text})
	}
	return b
}

// 添加下拉框元素
func (b *CardBuilder) Select(placeholder string, options []CardSelectOption, initValue string) *CardBuilder {
	if len(options) > 0 {
		b.card.Elements = append(b.card.Elements, CardSelect{
			Placeholder: placeholder, Options: options, InitValue: initValue,
		})
	}
	return b
}

// 使用给的buttons创建action row
func (b *CardBuilder) Buttons(buttons ...CardButton) *CardBuilder {
	if len(buttons) > 0 {
		b.card.Elements = append(b.card.Elements, CardActions{Buttons: buttons, Layout: CardActionLayoutRow})
	}
	return b
}

// 添加CardNoet
func (b *CardBuilder) TaggedNote(tag, text string) *CardBuilder {
	if text != "" {
		b.card.Elements = append(b.card.Elements, CardNote{Text: text, Tag: tag})
	}
	return b
}

// ====================== 辅助方法 ===========================

// 构建函数缩写
func Btn(text, typ, value string) CardButton {
	return CardButton{Text: text, Type: typ, Value: value}
}

// 创建默认样式button
func DefaultBtn(text, value string) CardButton {
	return CardButton{Text: text, Type: "default", Value: value}
}

// 创建一个 primary-styled button.
func PrimaryBtn(text, value string) CardButton {
	return CardButton{Text: text, Type: "primary", Value: value}
}

// 创建一个danger-styled button.
func DangerBtn(text, value string) CardButton {
	return CardButton{Text: text, Type: "danger", Value: value}
}

