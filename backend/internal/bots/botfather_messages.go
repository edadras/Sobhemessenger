package bots

import (
	"errors"
	"fmt"
	"strings"

	"github.com/sobh/messenger/backend/internal/httpx"
)

// What BotFather says (§13, §72).
//
// These are Persian, the platform's primary language, and they live here as
// constants rather than in the ARB files: this text is produced by the server
// and stored in a message row, so it is not something a client can re-render
// when the reader changes language. A bot's words are part of the
// conversation, and a conversation does not retranslate itself.

const botFatherDescription = "ساخت و مدیریت ربات‌های سُبح"

const botFatherAbout = "با من ربات بسازید، توکن بگیرید و تنظیماتش را عوض کنید. " +
	"برای شروع /newbot را بفرستید."

// botFatherCommands is the "/" menu it advertises.
var botFatherCommands = []Command{
	{Command: "newbot", Description: "ساخت یک ربات تازه"},
	{Command: "mybots", Description: "فهرست ربات‌های شما"},
	{Command: "token", Description: "گرفتن توکن تازه برای یک ربات"},
	{Command: "revoke", Description: "ابطال توکن‌های یک ربات"},
	{Command: "setname", Description: "تغییر نام نمایشی ربات"},
	{Command: "setdescription", Description: "تغییر توضیح ربات"},
	{Command: "setabout", Description: "تغییر متن دربارهٔ ربات"},
	{Command: "setcommands", Description: "تعیین فهرست دستورهای ربات"},
	{Command: "setprivacy", Description: "روشن یا خاموش کردن حالت حریم خصوصی"},
	{Command: "setinline", Description: "روشن یا خاموش کردن حالت درون‌خطی"},
	{Command: "setjoingroups", Description: "اجازهٔ افزوده شدن به گروه‌ها"},
	{Command: "deletebot", Description: "غیرفعال کردن یک ربات"},
	{Command: "cancel", Description: "لغو کار نیمه‌تمام"},
	{Command: "help", Description: "همین راهنما"},
}

const msgHelp = `سلام 👋

من ربات‌ساز سُبح هستم. با من می‌توانید ربات بسازید و آن را مدیریت کنید.

ساخت و فهرست
/newbot — ساخت یک ربات تازه
/mybots — فهرست ربات‌های شما

توکن
/token — گرفتن توکن تازه
/revoke — ابطال همهٔ توکن‌های یک ربات

تنظیمات
/setname — نام نمایشی
/setdescription — توضیح کوتاه
/setabout — متن «دربارهٔ»
/setcommands — فهرست دستورها
/setprivacy @نام on|off — حالت حریم خصوصی
/setinline @نام on|off — حالت درون‌خطی
/setjoingroups @نام on|off — افزوده شدن به گروه

/deletebot — غیرفعال کردن یک ربات
/cancel — لغو کار نیمه‌تمام`

const msgOnlyInPrivate = "من فقط در گفت‌وگوی خصوصی کار می‌کنم. " +
	"پاسخ‌های من توکن دارد و در گروه همه آن را می‌بینند."

const msgCancelled = "باشد، لغو شد."

const msgUnknownCommand = "این دستور را نمی‌شناسم. /help را بفرستید تا فهرست را ببینید."

const msgTemporaryProblem = "الان نتوانستم این کار را انجام دهم. کمی بعد دوباره امتحان کنید."

const msgSaved = "ذخیره شد. ✅"

const msgInactive = "غیرفعال"

const msgNoBotsYet = "هنوز رباتی نساخته‌اید. با /newbot شروع کنید."

const msgYourBots = "ربات‌های شما:"

const msgNotYourBot = "رباتی با این نام میان ربات‌های شما نیست."

const msgNewBotAskName = "بسیار خوب. اسم ربات چه باشد؟\n\n" +
	"این همان چیزی است که مردم بالای گفت‌وگو می‌بینند — مثلاً «دستیار هواشناسی»."

const msgNewBotBadName = "این اسم به کار نمی‌آید. یک اسم بین ۱ تا ۶۴ نویسه بفرستید."

const msgNewBotAskUsername = "حالا نام کاربری‌اش را بفرستید.\n\n" +
	"باید به bot ختم شود، بین ۴ تا ۳۲ نویسه باشد و فقط از a-z، ۰-۹ و _ استفاده کند.\n" +
	"مثال: weather_bot"

const msgNewBotBadUsername = "این نام کاربری معتبر نیست.\n\n" +
	"باید با یک حرف شروع شود، به bot ختم شود، و فقط a-z، ۰-۹ و _ داشته باشد.\n" +
	"دوباره بفرستید، یا /cancel برای انصراف."

const msgTokenAskWhich = "برای کدام ربات توکن تازه می‌خواهید؟"

const msgRevokeAskWhich = "توکن‌های کدام ربات باطل شود؟"

const msgSetNameAskWhich = "نام کدام ربات را عوض کنیم؟"

const msgSetDescriptionAskWhich = "توضیح کدام ربات را عوض کنیم؟"

const msgSetAboutAskWhich = "متن «دربارهٔ» کدام ربات را عوض کنیم؟"

const msgSetCommandsAskWhich = "دستورهای کدام ربات را تعیین کنیم؟"

const msgDeleteAskWhich = "کدام ربات غیرفعال شود؟"

// msgNewBotDone hands over the token, and says plainly that this is the only
// time it will be readable — the server stores only a hash.
func msgNewBotDone(handle, token string) string {
	return fmt.Sprintf(`ربات شما ساخته شد. 🎉

@%s

توکنش این است:

%s

⚠️ این توکن فقط همین یک بار نشان داده می‌شود. ما فقط هش آن را نگه می‌داریم، پس اگر گمش کنید بازیابی نمی‌شود و باید توکن تازه بگیرید.

آن را در جایی امن نگه دارید و هرگز در کد یا مخزن عمومی قرار ندهید.

توکن را در سرایند Authorization بفرستید:
    Authorization: Bearer %s

بعد به /setdescription و /setcommands نگاهی بیندازید.`, handle, token, token)
}

// msgNewBotRejected turns a validation failure into something a person can act
// on. The server's own message is used when there is one, because it says what
// is actually wrong — that the name is taken, or too long.
func msgNewBotRejected(err error) string {
	var apiErr *httpx.Error
	if errors.As(err, &apiErr) && apiErr.Message != "" {
		return apiErr.Message + "\n\nنام دیگری بفرستید، یا /cancel برای انصراف."
	}
	return msgTemporaryProblem
}

func msgTokenConfirm(handle string) string {
	return fmt.Sprintf(`توکن تازه برای @%s می‌سازم.

⚠️ توکن‌های فعلی این ربات باطل می‌شوند و هر چیزی که با آن‌ها کار می‌کند از کار می‌افتد.

اگر مطمئنید «بله» بفرستید، وگرنه /cancel.`, handle)
}

func msgTokenIssued(handle, token string) string {
	return fmt.Sprintf(`توکن تازهٔ @%s:

%s

⚠️ فقط همین یک بار نشان داده می‌شود. توکن‌های قبلی دیگر کار نمی‌کنند.`, handle, token)
}

func msgDeleteConfirm(handle string) string {
	return fmt.Sprintf(`می‌خواهید @%s را غیرفعال کنید.

ربات غیرفعال دیگر پیامی نمی‌گیرد و همهٔ توکن‌هایش از کار می‌افتند. پیام‌هایی که فرستاده سر جایشان می‌مانند.

برای تأیید، دقیقاً نام کاربری‌اش را بفرستید: %s

یا /cancel برای انصراف.`, handle, handle)
}

func msgDeleted(handle string) string {
	return fmt.Sprintf("@%s غیرفعال شد.", handle)
}

func msgSetCommandsBad(err error) string {
	return fmt.Sprintf(`فهرست دستورها را نتوانستم بخوانم: %v

هر خط باید این شکل باشد:

start - شروع کار
help - راهنما

دوباره بفرستید، یا /cancel برای انصراف.`, err)
}

// promptForFlow asks the question a targeted flow needs, naming the bot so the
// user can see which one they are about to change.
func promptForFlow(flow string, bot Bot) string {
	handle := ""
	if bot.Username != nil {
		handle = "@" + *bot.Username
	}

	switch flow {
	case flowSetName:
		return fmt.Sprintf("نام تازهٔ %s چه باشد؟\n\nنام فعلی: %s",
			handle, bot.DisplayName)
	case flowSetDescription:
		return fmt.Sprintf("توضیح تازهٔ %s را بفرستید.\n\n"+
			"این متن را کسی می‌بیند که هنوز گفت‌وگو را شروع نکرده است.", handle)
	case flowSetAbout:
		return fmt.Sprintf("متن «دربارهٔ» %s را بفرستید.\n\n"+
			"این متن در نمایهٔ ربات دیده می‌شود.", handle)
	case flowSetCommands:
		return fmt.Sprintf(`فهرست دستورهای %s را بفرستید، هر دستور در یک خط:

start - شروع کار
help - راهنما
settings - تنظیمات

فهرست تازه جای فهرست قبلی را می‌گیرد.`, handle)
	default:
		return msgHelp
	}
}

// msgFlagUsage explains a setting that takes an on or an off.
func msgFlagUsage(command string) string {
	return fmt.Sprintf("این‌طور بفرستید:\n\n/%s @نام_ربات on\n/%s @نام_ربات off",
		command, command)
}

// msgFlagSet confirms a change, and says what it means rather than only that
// it happened — "privacy off" is the setting people most often regret.
func msgFlagSet(flag, handle string, enabled bool) string {
	state := "خاموش"
	if enabled {
		state = "روشن"
	}

	var explanation string
	switch flag {
	case "privacy":
		if enabled {
			explanation = "حالا فقط پیام‌هایی را می‌بیند که خطاب به اوست: دستورها، پاسخ‌ها و منشن‌ها."
		} else {
			explanation = "⚠️ حالا همهٔ پیام‌های هر گروهی را که در آن باشد می‌بیند."
		}
	case "inline":
		if enabled {
			explanation = "حالا می‌شود از جعبهٔ نوشتن هر گفت‌وگویی صدایش زد."
		} else {
			explanation = "دیگر از جعبهٔ نوشتن صدا زده نمی‌شود."
		}
	case "joingroups":
		if enabled {
			explanation = "حالا می‌شود او را به گروه افزود."
		} else {
			explanation = "دیگر نمی‌شود او را به گروه افزود."
		}
	}

	return fmt.Sprintf("@%s — %s\n\n%s",
		handle, strings.TrimSpace(state), explanation)
}
