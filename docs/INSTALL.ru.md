# Установка lidwake

[In English](INSTALL.md)

## Требования

- macOS 14 Sonoma или новее, Apple Silicon или Intel.
- Учётная запись администратора: установка один раз спрашивает пароль, чтобы поставить root-хелпер.
- Только для сборки самому: Go 1.26+ и Xcode Command Line Tools (`xcode-select --install`).

## 1. Установка

Выбери один способ.

### Готовый бинарь (рекомендуется)

```sh
curl -fsSL https://raw.githubusercontent.com/nikitaShakhbazyan/lidwake-go/main/install.sh | bash
```

Скрипт скачивает последний [релиз](https://github.com/nikitaShakhbazyan/lidwake-go/releases) —
один универсальный бинарь для Apple Silicon и Intel, — сверяет его SHA-256 с `checksums.txt`
релиза и запускает `lidwake setup`. Зафиксировать версию:
`curl -fsSL …/install.sh | LIDWAKE_VERSION=0.1.0 bash`.

### Через Go

```sh
go install github.com/nikitaShakhbazyan/lidwake-go/cmd/lidwake@latest
"$(go env GOPATH)/bin/lidwake" setup
```

### Из исходников

```sh
git clone https://github.com/nikitaShakhbazyan/lidwake-go.git
cd lidwake-go
make install
```

## 2. Что делает `lidwake setup`

Сначала спрашивает пароль, затем:

1. останавливает запущенный демон lidwake, если он есть (это обновление);
2. копирует бинарь в `/usr/local/libexec/lidwake/lidwake`, владелец — root, и подписывает его
   ad-hoc с hardened runtime (бинари из релизов приходят уже подписанными);
3. создаёт ссылку `/usr/local/bin/lidwake`;
4. проверяет, что каждый каталог на этом пути доступен на запись только root — иначе хелпер не
   будет доверять демону, — и предупреждает, если это не так;
5. регистрирует и запускает root-хелпер:
   `/Library/LaunchDaemons/io.github.nikitashakhbazyan.lidwake.helper.plist`;
6. регистрирует и запускает твой демон:
   `~/Library/LaunchAgents/io.github.nikitashakhbazyan.lidwake.daemon.plist`;
7. обновляет уже установленные хуки lidwake, если они указывают на старый бинарь.

Повторный запуск обновляет установку на месте. macOS может показать уведомление
*«Добавлены фоновые объекты»* для хелпера и демона — так и должно быть.

## 3. Подключи агентов

```sh
lidwake install-hooks --dry-run   # посмотреть, что изменится
lidwake install-hooks             # все найденные агенты
lidwake install-hooks --tool claude-code
```

Агенты: `claude-code`, `codex`, `cursor`, `gemini-cli`, `aider`, `cline`, `hermes`, `opencode`,
`pi`. Добавляются только записи lidwake (с пометкой `_lidwake`), остальное в конфигах не трогается.

- **Codex** запускает хук только после подтверждения: открой Codex и одобри хуки lidwake в `/hooks`.
- Перезапусти уже открытые сессии агентов, чтобы они подхватили хуки.
- Без хуков: `lidwake run -- <команда>` держит мак бодрствующим, пока работает любая команда.

## 4. Проверь, что работает

```sh
lidwake daemon-status    # daemon: running
lidwake stats            # без предупреждения «privileged helper is not connected»
```

Настоящая проверка — с крышкой:

```sh
lidwake run -- sleep 300       # терминал 1: держит мак 5 минут
pmset -g | grep SleepDisabled  # терминал 2: пока работает — SleepDisabled 1
```

Закрой крышку на минуту и открой: `lidwake stats` по-прежнему показывает удержание, а в
`pmset -g log | grep "Entering Sleep"` нет новой записи. Когда `sleep 300` закончится,
`SleepDisabled` вернётся к прежнему значению (`0`, если ты не ставил его сам).

## 5. Повседневное использование

```sh
lidwake stats          # дашборд: пробел вкл/выкл, t таймер, r отпустить всё, q выход
lidwake off | on       # перестать держать мак или снова разрешить
lidwake timer 1h       # выключиться самому через час
lidwake config         # настройки: порог батареи, отсечка по температуре, только от сети…
```

Все команды и настройки — в [README](../README.md).

## 6. Обновление

Запусти ту же команду установки ещё раз. Setup заменит бинарь, перезапустит хелпер и демон и
сохранит твои настройки и хуки.

## 7. Удаление

```sh
lidwake uninstall
```

Сначала спрашивает пароль, затем удаляет хуки из всех агентов, демон, хелпер и бинарь и возвращает
настройку сна. Настройки и логи остаются; удалить и их:

```sh
rm -rf ~/Library/Application\ Support/lidwake ~/Library/Logs/lidwake
```

## Если что-то не так

**`lidwake: command not found`** — открой новый терминал. `/usr/local/bin` входит в стандартный
`PATH` macOS; если твой его заменяет — добавь обратно.

**«The privileged helper is not connected»** — проверь хелпер:
`sudo launchctl print system/io.github.nikitashakhbazyan.lidwake.helper | head` и его лог
`/var/log/lidwake-helper.log`, затем снова запусти `lidwake setup`.

**Setup пишет «… the helper will not trust the daemon»** — какой-то каталог на пути
`/usr/local/libexec/lidwake/lidwake` — часто `/usr/local` или `/usr/local/libexec` после старой
установки Homebrew — доступен на запись обычному пользователю. Верни его root:
`sudo chown root:wheel <каталог> && sudo chmod go-w <каталог>` и снова запусти `lidwake setup`. На
Intel-маке, где Homebrew живёт в `/usr/local`, меняй только тот каталог, который назван в
предупреждении.

**macOS не открывает бинарь, скачанный через браузер** — сними флаг карантина:
`xattr -d com.apple.quarantine ./lidwake`. Установку через curl это не касается.

**Setup падает на «sign the binary»** — для подписи нужны Xcode Command Line Tools
(`xcode-select --install`). Бинари из релизов уже подписаны, поэтому при установке готового
бинаря подпись не нужна.

**Мак всё равно засыпает с закрытой крышкой** — открой `lidwake stats`:
- *OFF* → `lidwake on`;
- агентов нет в списке → хуки не установлены (шаг 3) или сессия запущена раньше них;
- *CUTOUT* → сработала защита: заряд на пороге или ниже, режим «только от сети» на батарее или
  CPU перегрет; снимется сама, когда условие пройдёт;
- предупреждение о хелпере → см. выше.

**Мак не засыпает после окончания работы** — `lidwake status` покажет, что его держит;
`lidwake release --all` снимет всё. Крайний случай: `sudo pmset -a disablesleep 0`.

**Логи** — `~/Library/Logs/lidwake/daemon.log`, `/var/log/lidwake-helper.log` и журнал событий
`~/Library/Application Support/lidwake/events.log`.

> Закрытый MacBook под постоянной нагрузкой в сумке сильно греется. Отсечка по температуре —
> страховка, а не охлаждение: оставляй его там, где есть воздух.
