# Prima Shade CRM – předávací návod

Tento soubor popisuje, co tu je, kde to běží a jak se na tom dál pracuje.
Je psaný pro Petra i pro Clauda: **v nové konverzaci stačí Claudovi nahrát zip
s programem (nebo mu dát repozitář) a napsat „přečti si PREDAVACI-NAVOD.md a pokračujeme“.**

Komunikace s Petrem: česky, stručně. Vizuální změny (vzhled, fotky) nejdřív jako
screenshot ke schválení, nasazovat až po souhlasu.

---

## Co to je

Vlastní CRM pro firmu Prima Shade (prodej a montáž pergol): leady, klienti,
obchodní případy, nabídky (PDF + online nabídka s odkazem), kalendář, montáže,
ceník, sklad (včetně naskladnění z faktury ISDOC), pošta (IMAP/SMTP schránky),
e-mailové šablony, automatizace, novinky na nástěnce, **Účetnictví** (vlastní fakturace místo Fakturoidu,
přijaté doklady, banka, export pro účetní).

## Kde co je

| Co | Kde |
|---|---|
| Zdrojový kód | GitHub `petrsimanekk-gif/travnik` |
| Běžící CRM | `https://crm.primashade.cz` na Ubuntu VPS |
| Na serveru | program `/opt/stinko/stinko`, data `/opt/stinko/pb_data`, zdroják `/opt/stinko/src` |
| Zálohy dat | každou noc 02:30 do `/var/backups/stinko` (14 dní, **jen na stejném serveru**); záloha mimo server se zapne vyplněním `/etc/stinko/backup.env` (restic) |

## Struktura repozitáře

- `crm/index.html` – **celé CRM v jednom souboru** (HTML + CSS + JS, bez build kroku). Edituje se přímo.
- `crm/stinko-server.js` – přihlášení a API klienta proti vlastnímu serveru.
- `crm/nabidka.html` – veřejná stránka online nabídky (`/n/{token}`).
- `crm/poptavka.html` – poptávkový formulář pro web.
- `crm/offer/` – podklady PDF nabídky (předlohy, fonty, obrázky).
- `server/` – server v Go nad PocketBase:
  - `main.go` – start, přihlášení, ukládání záznamů, pravidla zápisu (`canWrite`), soubory.
  - `mail.go`, `mail2.go`, `mail3.go` – schránky, odesílání (`composeMail`), koncepty, koš.
  - `offerlink.go` – online nabídka, e-mail s nabídkou, sledování návštěv.
  - `accounting.go` – Účetnictví: číslování faktur, PDF a e-mail k faktuře, úhrady, storno, ARES; zámek účetních dat.
  - `airead.go` – čtení přijatých dokladů (fotka/PDF) přes OpenAI nebo Anthropic, klíče v `secrets/ai`.
  - `fakturoid.go` – vystavení záloh/faktur ve Fakturoidu a kontrola úhrad (po zapnutí Účetnictví se už nepoužívá).
  - `auto.go` – automatizace; `richtext.go` – formátování e-mailů; `weblead.go` – poptávky z webu.
  - `install.sh` – instalace i aktualizace serveru (vytváří i příkaz `stinko-update`).

## Větve a nasazení

- **`claude/bold-brown-vhwapp` = to, co běží na serveru.** VPS si stahuje právě tuto větev.
- Práce se dělá v pracovní větvi (název přidělí Claude Code v dané session), hotové commity
  se přenesou do `claude/bold-brown-vhwapp` (cherry-pick přes `git worktree`).
- Nasazení spouští Petr sám na VPS:
  ```bash
  sudo stinko-update
  ```
  Kontrola nasazené verze:
  ```bash
  git -C /opt/stinko/src log --oneline -1
  ```
  Po nasazení v prohlížeči Ctrl+F5.
- Úplně nový server: `curl -fsSL https://raw.githubusercontent.com/petrsimanekk-gif/travnik/claude/bold-brown-vhwapp/server/install.sh | sudo bash -s -- crm.primashade.cz`
  (data se pak obnoví ze zálohy: zastavit `stinko`, nahrát `data.db` a `storage` do `/opt/stinko/pb_data`, spustit).

## Jak ověřit změny (pro Clauda)

```bash
cd server && GOTOOLCHAIN=auto go vet . && GOTOOLCHAIN=auto go test . && GOTOOLCHAIN=auto CGO_ENABLED=0 go build -o /tmp/stinko .
```
Prohlížečové testy: spustit server s `STINKO_PUBLIC=<kopie crm>` a `--dir <prázdná složka>`,
v Playwrightu založit admina (`#su-name`, `#su-user`, `#su-pw`, `#su-pw2`), data vkládat přes
háček `window.__t = s => eval(s)` vložený před `boot();` v kopii `index.html` (`save("products", {...})`, `go("stock")`).
Bez serveru jde `index.html` otevřít i samostatně (režim bez ukládání, data v paměti).

## Účetnictví (sekce v menu)

- Vidí ji jen admin a ten, komu admin dá v Nastavení → Uživatelé právo „Účetnictví“ (Vidí / Upravuje).
  Role **Účetní** = jen Účetnictví. Zamčené i na serveru: `OnRecordEnrich` v `main.go` vrací záznamy `acc_*`
  ostatním jako `{"_hidden": true}`, zápis hlídá `writeSections` (sekce `accounting`).
- Data (kolekce v `docs`): `acc_out` vydané faktury, `acc_in` přijaté doklady, `acc_bank` pohyby z výpisů,
  `acc_cfg/main` údaje firmy, účty, číslování, DPH. Po uložení Nastavení se do `config/main` zapíše
  `invoicing:"crm"` a tlačítko 🧾 v obchodním případu pak vystavuje přes Účetnictví (ne Fakturoid).
- Číslování: `2026001` (faktury), `Z2026001` (zálohy). VS = číslice čísla, záloha s „1“ na začátku (`12026001`),
  aby se nikdy neshodoval. Číslo přiděluje server (`/api/stinko/acc/issue`, zámek `accMu`).
- PDF faktury s QR Platbou (SPAYD) se tvoří v prohlížeči (pdf-lib, `accPdf`), nahraje se a připojí přes
  `/api/stinko/acc/attach` (ten zároveň připraví e-mail do Konceptů ze šablony „Zálohová faktura“ / „Faktura“).
- Úhrada (`/acc/paid`) a storno (`/acc/storno`) upraví i fakturu v obchodu; uhrazená záloha posune obchod na „ZF uhrazena“.
- Přijaté doklady: nahrání PDF/fotek/ISDOC (přetažením, víc najednou, fotka z mobilu). ISDOC se vyplní sám
  (`isdocHead`). Fotky a PDF bez ISDOC **přečte umělá inteligence** (`server/airead.go`, `/api/stinko/acc/read`):
  údaje dokladu i položky (`acc_in.items`) se předvyplní, doklad zůstane „Ke zpracování“, dokud ho člověk
  nezkontroluje a neuloží. V detailu dokladu tlačítka „Přečíst údaje z dokladu“ a „Přečíst pečlivěji“ (dražší model).
  Nastavení (jen admin): Účetnictví → Nastavení → „Čtení dokladů z fotky a PDF“ – služba OpenAI / Anthropic,
  API klíče (uložené jen na serveru, `secrets` klíč `ai`), modely. Výchozí: OpenAI `gpt-5-mini` / pečlivěji
  `gpt-5.4-mini`; Anthropic `claude-haiku-5-5` / `claude-sonnet-5-5`. Cena se počítá podle tabulky `aiPrices`
  (kurz 23 Kč/USD) a sčítá po měsících. Petr má kredit u OpenAI, po jeho vyčerpání přepne na Anthropic.
  Testy: `airead_test.go` (simulované API přes `STINKO_OPENAI_URL` / `STINKO_ANTHROPIC_URL`).
  **Sklad → „📄 Naskladnit z dokladu“** používá stejné čtení: fotka účtenky / PDF bez ISDOC → položky
  (`stockInvFromAi`; nákupní ceny s DPH, dokud firma není plátce – `stockVat`, pak bez DPH) → spárování s produkty → naskladnění; zaškrtnutím se doklad
  zapíše i do Přijatých dokladů. Z detailu přijatého dokladu s položkami tlačítko „📦 Naskladnit položky“.
  Naskladněný doklad má `acc_in.stocked = {at, by, n}`.
  Dlouhé účtenky: `server/imgprep.go` vytáhne fotku ze skenovaného PDF (bez textu, jeden JPEG – CamScanner apod.)
  a vysoký obrázek rozřeže na části (řezy v bílých mezerách mezi řádky), jinak by model četl zmenšené písmo a pletl
  množství a ceny. `cleanAiDoc` hlídá množství × cena = celkem a upozorní, když součet položek nesedí s celkem. Čtení smí spustit i kdo má jen právo upravovat Sklad (`aiStockEditor`).
- Banka: **platby se zapisují samy z e-mailových oznámení banky** (`server/bankmail.go`): ČSOB CEB posílá
  „Zaúčtování platby“ (Kredit i Debet, 1–99 999 999 Kč) na faktury@primashade.cz, schránka je připojená v Poště;
  při synchronizaci schránky `bankFromMail` oznámení rozpozná, uloží do `acc_bank` (source "mail") a spáruje podle VS
  (`bankAutoMatch` → `accMarkPaid`), lidem s Účetnictvím přijde upozornění. Ověřeno na skutečném vzorku ČSOB;
  **Moneta: vzorek oznámení zatím chybí**, parser je obecný (štítky „Částka:“, „Variabilní symbol:“), ověřit.
  Kontrolně jde nahrát výpis **GPC (ABO)** (`parseGpc`); platby, které už přišly e-mailem, se nezdvojí.
- Export: ZIP za období = PDF + ISDOC vydaných faktur, originály přijatých dokladů, `Prehled.xlsx`.
- Neplátce DPH: hlídání obratu (limit 2 000 000 Kč/rok). Přepnutí na plátce v Nastavení (datum „od“)
  zapne DPH na nových fakturách. **Zbývá:** daňový doklad k přijaté záloze, podklady k přiznání DPH
  a kontrolnímu hlášení, faktury z e-mailu jedním klikem v Poště, ISDOC ověřit u účetní (Pohoda/Money).

## Tajné údaje

V repozitáři **nejsou žádná hesla**. Hesla schránek, Fakturoid Client ID/Secret apod. jsou uložené jen
v databázi na serveru (kolekce `secrets`, `mailboxes`) a zadávají se v CRM v Nastavení.

## Stav k 28. 10. 2026

Hotovo a nasazeno (`claude/bold-brown-vhwapp` @ `8e7cee3`):
- nový vzhled „kompaktní pracovní“ (písmo IBM Plex Sans, bez velkých písmen, husté tabulky), klik na logo = Nástěnka,
- Sklad: souhrnné dlaždice, sloupec Stav zásoby, naskladnění z faktury (ISDOC soubor / ISDOC v PDF / ruční zadání), bez AI,
- dříve: online nabídka, Fakturoid, Pošta (koncepty, koš), novinky, formátování e-mailů.

Otevřené / na později:
- záloha dat mimo server (restic → např. Backblaze B2 / Google Drive),
- Fakturoid otestovat proti skutečnému účtu (slug `primashade`),
- kam posunout obchod po úhradě konečné faktury,
- telefon 123456789 natvrdo v PDF předloze,
- zabezpečení SSH, šifrování hesel schránek, automatická aktualizace serveru,
- logo a údaje firmy v PDF, ceny doplňků, tabulka screen rolet, ICS a připomínky.
