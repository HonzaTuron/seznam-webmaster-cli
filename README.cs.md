# szwm: CLI pro Seznam Webmaster

[![Licence: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

[English](README.md) | **Čeština**

Nástroj pro příkazovou řádku a MCP server pro API [Seznam.cz Webmaster](https://reporter.seznam.cz/wm/), navržený především pro AI agenty. Zjistíte, jak Seznam vidí váš web, zkontrolujete indexaci a požádáte o reindexaci, ať už z terminálu, cronu, nebo přes AI agenta.

> Jde o **neoficiální** projekt. Společnost Seznam.cz, a.s., ho nevytvořila ani nepodporuje.

## Proč další obal nad API?

Většina CLI nástrojů vzniká pro lidi a stroje jejich výstup parsují až dodatečně. `szwm` vzniká opačně, podle toho, co se rozbíjí, když nástroj ovládá agent:

| Problém agentů | Co dělá szwm |
|---|---|
| Výstup se mezi běhy mění | Jedna stabilní JSON obálka verzovaná přes `schema_version` |
| Chyby se od sebe nedají rozlišit | 19 stabilních chybových kódů, 13 návratových kódů, navíc `retryable` a `hint` |
| Nástroj se zasekne na čekání na vstup | Nikdy se na nic neptá: hromadný zápis bez `--yes` hned skončí chybou |
| Limity se překračují ve smyčce samostatných procesů | Limity platí **napříč procesy** pro každý API klíč |
| Denní kvóta dojde uprostřed dávky | Dávka se zkrátí na zbývající kvótu a zbytek se nahlásí po jednotlivých URL |
| Odpovědi zahltí kontext | `--fields`, `--counts-only`, `--brief`, výchozí limity URL, streamování NDJSON |
| Tajné údaje unikají do logů | Žádný přepínač `--api-key`. Klíč se odstraňuje z chyb i z trasování |
| Stažené stránky mohou obsahovat prompt injection | `meta.untrusted_fields` označí každé pole z cizích stránek |
| Dokumentace se rozchází se skutečností | `szwm schema` se generuje přímo ze stromu příkazů |
| API se potichu změní | `szwm doctor` porovná živou specifikaci s vestavěným snímkem |

## Instalace

S Go 1.26 nebo novějším:

```bash
go install github.com/HonzaTuron/seznam-webmaster-cli/cmd/szwm@latest
```

Předpřipravené binárky budou na stránce [Releases](https://github.com/HonzaTuron/seznam-webmaster-cli/releases), jakmile vyjde první verze.

## Rychlý start

1. Ve [Webmasteru](https://reporter.seznam.cz/wm/) vyberte web a zkopírujte klíč v části **API > Klíč k API**.
2. Uložte ho. Čte se ze standardního vstupu, takže se nedostane do historie shellu:

   ```bash
   printf %s "$KEY" | szwm auth login --profile shop --site example.com
   ```

3. Ověřte, že vše funguje, a podívejte se na web:

   ```bash
   szwm doctor
   szwm report
   ```

Ve skriptech a CI se profil nemusí používat, stačí nastavit `SZWM_API_KEY`.

## Příkazy

| Příkaz | Co dělá | Efekt | Klíč |
|---|---|---|---|
| `report` | Souhrn stavu webu se stabilními kódy nálezů. **Začněte tady** | čtení | ano |
| `web` | Počty, vzorky URL a historie | čtení | ano |
| `documents` | Počty podle kategorií s náhodným vzorkem URL | čtení | ano |
| `history` | Denní počty v čase | čtení | ano |
| `document [url...]` | Jak Seznam vidí konkrétní stránky, zvládá i dávky | čtení | ano |
| `reindex [url...]` | Žádost o reindexaci | **zápis** | ano |
| `database-info` (`db`) | Kdy byla data naposledy vydána | čtení | ne |
| `quota` | Místní odhad dnešní kvóty na reindexaci | lokální | ne |
| `api <METHOD> <path>` | Přímé volání API včetně autentizace, limitů a opakování | různý | různý |
| `auth login\|status\|list\|use\|logout` | Správa profilů jednotlivých webů | lokální | |
| `doctor` | Diagnostika nastavení, klíče, připojení a změn specifikace | čtení | ne |
| `schema [příkaz]` | Strojově čitelný kontrakt | lokální | ne |
| `guide` | Návod pro agenty, `--install-skill` ho nainstaluje | lokální | ne |
| `mcp` | Spustí MCP server na stdio | čtení/zápis | |
| `version`, `completion` | Informace o verzi a doplňování v shellu | lokální | ne |

Mezi globální přepínače patří `--profile`, `-o/--output`, `--fields`, `--raw`, `--pretty`, `--read-only`, `--timeout`, `--deadline`, `--retries` a `--verbose`. Úplný seznam vypíše `szwm schema --fields global_flags`.

## Recepty

```bash
# Je něco špatně? Od úrovně warning vrací kód 12, což se hodí pro cron.
szwm report --window 30 --fail-on warning

# Které stránky mají chyby? Jde o náhodný vzorek, ne o úplný seznam.
szwm documents --category error --limit 50

# Proč tahle stránka není na Seznamu?
szwm document https://example.com/stranka --brief

# Počty stránek do tabulky
szwm history --last 90d -o csv > historie.csv

# Audit sitemapy. Výsledky průběžně přibývají, zhruba 100 URL za minutu.
szwm document --sitemap https://example.com/sitemap.xml --brief -o ndjson > audit.ndjson

# Reindexace jen toho, co chybí: nejdřív náhled, potom odeslání.
jq -c 'select(.type == "item" and .state == "not_indexed")' audit.ndjson > todo.ndjson
szwm reindex --file todo.ndjson --dry-run --fields summary,quota
szwm reindex --file todo.ndjson --yes --deadline 10m

# Volání endpointu, který nemá vlastní příkaz
szwm api GET /web/document --param url=https://example.com/
```

## Pro AI agenty

### Kontrakt

- Výstup je JSON vždy, když stdout není terminál. Změníte to přes `-o` nebo `SZWM_OUTPUT`.
- V režimu `json` je na stdout **právě jeden dokument**, při úspěchu i při chybě.
- V režimu `ndjson` je každá položka na samostatném řádku s `"type":"item"` a poslední řádek má `"type":"summary"`. Položky se streamují, takže ukončený běh si zachová, co stihl.
- Při chybě jde na stderr navíc jednořádková zpráva, aby byla vidět i při přesměrování stdout.
- Návratový kód 0 znamená vždy `ok: true`.

```json
{
  "schema_version": "1",
  "ok": false,
  "command": "web",
  "error": {
    "code": "auth_invalid",
    "message": "Seznam rejected the API key: API key does not exist.",
    "hint": "Get the current key under Webmaster > API > Klíč k API, then run `szwm auth login`.",
    "retryable": false,
    "http_status": 401,
    "exit_code": 3
  },
  "meta": { "szwm_version": "0.1.0", "duration_ms": 127, "requests": 1, "retries": 0 }
}
```

I `ok: false` může obsahovat `data`: částečně dokončená dávka přesně říká, které URL prošly. V `meta` se mohou objevit také `truncated`, `hints`, `warnings` a `untrusted_fields`.

Zprávy nástroje jsou anglicky, aby na nich agenti i skripty mohli stavět napříč projekty. Pro rozhodování ale vždy používejte `error.code`, nikdy text zprávy.

### Návratové kódy

| Kód | Chybové kódy | Význam | Opakovat? |
|---|---|---|---|
| 0 | | Úspěch | |
| 1 | `internal`, `api_error`, `doctor_failed` | Neočekávaná chyba nebo neúspěšná kontrola `doctor` | ne |
| 2 | `invalid_argument`, `confirmation_required` | Opravte příkaz | ne |
| 3 | `auth_missing`, `auth_invalid` | Chybějící nebo odmítnutý klíč | ne |
| 4 | `forbidden` | URL mimo tento web nebo chybí právo zápisu | ne |
| 5 | `not_found` | Seznam k tomu nemá data | ne |
| 6 | `no_data_yet` | Dotaz je v pořádku, data ještě nejsou | později |
| 7 | `rate_limited` | Počkejte `retry_after_seconds` | ano |
| 8 | `service_unavailable`, `network_error`, `timeout`, `bad_response` | Přechodná chyba | ano |
| 9 | `partial_failure` | Dávka doběhla jen částečně | neúspěšné položky |
| 10 | `quota_exhausted` | Denní kvóta na reindexaci je vyčerpaná | zítra |
| 11 | `read_only` | Režim jen pro čtení zablokoval zápis | ne |
| 12 | `findings_threshold` | Splnila se podmínka `report --fail-on` | ne |

### Popis sebe sama

```bash
szwm schema                                    # celý kontrakt
szwm schema reindex                            # jeden příkaz: přepínače, efekt, volání API, příklady
szwm schema --fields commands.command,commands.effect
szwm guide                                     # návod psaný pro agenty
```

### Úspora tokenů

```bash
szwm web --counts-only                         # bez vzorků URL
szwm documents --category error --limit 20     # jedna kategorie, málo URL
szwm document --stdin --brief --fields summary < urls.txt
szwm doctor --fields checks.name,checks.status
```

`--fields` přijímá cesty s tečkami a umí procházet pole, takže `items.url` vybere `url` z každé položky. Seznamy URL mají výchozí limit 100 na kategorii a `meta.truncated` prozradí, že se něco zkrátilo.

### Soubor se skillem

Návod pro agenty je zabudovaný přímo v binárce a v repozitáři leží v [`skills/seznam-webmaster/SKILL.md`](skills/seznam-webmaster/SKILL.md):

```bash
szwm guide --install-skill ~/.claude/skills
```

### MCP server

`szwm mcp` spustí MCP server na stdio. Nejde o produkt Seznamu: server stojí nad jejich REST API a má stejné řízení limitů, sledování kvóty i bezpečnostní kontroly jako CLI.

Claude Code:

```bash
claude mcp add seznam-webmaster -- szwm mcp --read-only
```

Ostatní klienti:

```json
{
  "mcpServers": {
    "seznam-webmaster": {
      "command": "szwm",
      "args": ["mcp", "--read-only"]
    }
  }
}
```

Pokud chcete povolit reindexaci, odeberte `--read-only`. V režimu jen pro čtení se zapisovací nástroj vůbec nezaregistruje, takže ho klient ani neuvidí.

| Nástroj | Co dělá | Zapisuje |
|---|---|---|
| `health_report` | Stav webu s nálezy. Začněte tady | ne |
| `database_info` | Čas vydání dat | ne |
| `site_overview` | Počty a historie, URL jen na vyžádání | ne |
| `list_documents` | Počty s náhodným vzorkem URL | ne |
| `documents_history` | Denní počty | ne |
| `get_document` | Jedna stránka | ne |
| `check_documents` | Až 100 stránek | ne |
| `quota_status` | Místní odhad kvóty na reindexaci | ne |
| `list_profiles` | Nastavené weby, bez klíčů | ne |
| `reindex_urls` | Až 100 stránek, s `dry_run` a `confirm_bulk` | **ano** |

## Weby a profily

API nemá parametr pro výběr webu, web určuje klíč. Pro každý web si proto založte jeden profil:

```bash
printf %s "$KEY_A" | szwm auth login --profile shop --site example.com --default
printf %s "$KEY_B" | szwm auth login --profile blog --site example.org
szwm report --profile blog
szwm report --profile example.org    # funguje i název webu
```

Klíč se hledá v tomto pořadí a použije se první nalezený:

1. `--api-key-file` (`-` znamená stdin)
2. `--profile`
3. `SZWM_API_KEY`, potom `SEZNAM_WEBMASTER_API_KEY`
4. `SZWM_PROFILE`
5. Výchozí profil, případně jediný existující profil

## Limity a kvóta

Seznam uvádí tyto limity, ověřované pro každého uživatele a každý web:

- 5 dotazů za vteřinu
- 100 dotazů za minutu
- 500 žádostí o reindexaci za den

`szwm` dotazy rozkládá v čase pomocí malého stavového souboru pro každý klíč, takže limity dodrží i smyčka v shellu, která pro každou URL spouští nový proces. Pokud stavový soubor nejde zapsat, omezuje dotazy aspoň v rámci procesu a volání API proběhne.

Přechodné chyby (429, 502, 503, 504, síťové chyby, timeouty) se opakují s exponenciálním odstupem a náhodným rozptylem. `Retry-After` se respektuje až do 60 sekund, delší čekání vrátí `rate_limited` s `retry_after_seconds`.

API nemá endpoint pro kvótu, takže `szwm quota` je jen **odhad** z evidence na tomto počítači. Dotazy odjinud se do něj nepočítají. Seznam neuvádí, kdy den začíná, proto `szwm` předpokládá půlnoc v Europe/Prague. Pokud víte víc, použijte `--ignore-quota`.

## Report o stavu webu

`szwm report` udělá dvě volání API a vrátí součty, poměry, trend a nálezy se stabilními kódy. Celková `severity` odpovídá nejhoršímu nálezu.

| Kód | Závažnost | Kdy nastane |
|---|---|---|
| `nothing_indexed` | critical | Stránky se stáhly, ale žádná není v indexu |
| `low_index_ratio` | warning | V indexu je méně než 50 % stažených stránek |
| `high_error_ratio` | warning / critical | Chyby tvoří aspoň 10 % / 25 % procházených stránek |
| `index_drop` | warning / critical | Počet stránek v indexu klesl za sledované období aspoň o 10 % / 30 % |
| `error_rise` | warning / critical | Chyb přibylo aspoň 10 a zároveň 20 % / 100 %, nebo se objevily z nuly |
| `stale_history` | warning | Nejnovější řádek historie je starší než 4 dny |
| `stale_release` | warning | Vydání dat je starší než 3 dny |
| `no_history` | info | Na trend není dost historie |

Tyto prahy jsou heuristiky tohoto nástroje, ne pravidla Seznamu.

## Bezpečnost

- **Žádný klíč na příkazové řádce.** Přepínače unikají do seznamu procesů a logů, proto `--api-key` neexistuje.
- **Klíč musí cestovat v URL**, protože tak API funguje. `szwm` ho odstraňuje z chybových zpráv i z trasování `--verbose`.
- **Uložené klíče** jsou v konfiguračním souboru s právy `0600` jako prostý text. `doctor` selže, pokud je soubor čitelný pro ostatní.
- **Místní stav** obsahuje jen otisk klíče, nikdy klíč samotný.
- **Přesměrování se nikdy nenásledují**, protože by tím klíč mohl odejít jinam.
- **Základní URL je omezená** na `https://reporter.seznam.cz` a loopback, takže zmanipulovaný agent nemůže poslat klíč na jiný server.
- **Stahování sitemap používá samostatného HTTP klienta**, který ke klíči nemá přístup.
- **Stažený obsah je nedůvěryhodný.** `title`, `meta`, `openGraphData` a `responseHeaders` pocházejí z webových stránek a mohou obsahovat text cílený na agenta. `meta.untrusted_fields` je vyjmenuje.
- **`--read-only`** nebo `SZWM_READ_ONLY=1` zablokuje všechny zápisy, i přes `szwm api`.

## Co vědět o API

Není-li uvedeno jinak, ověřeno proti živé službě:

- `basePath` ve specifikaci je `/wm-api/wm-api`, ale funkční prefix je `/wm-api`.
- Chyby obsahují pole `detail`, zatímco zveřejněné schéma `ProblemResult` uvádí `description`. `szwm` čte obojí.
- Dokumentace zmiňuje kategorii `doc_count`, která ve zveřejněných modelech chybí. `szwm` neznámá pole propouští. Její přesná podoba není potvrzená, proto přijímá objekt i prosté číslo.
- `urls` je **náhodný vzorek** nejvýše 1 000 URL na kategorii. Dva běhy se mohou lišit, takže pro součty použijte `count` a pro úplný soupis `--sitemap`.
- Data se mění jen s vydáním (`szwm database-info`). Žádost o reindexaci se projeví až v některém pozdějším vydání, takže opakované dotazování v rámci jednoho vydání nemá smysl.
- Repliky hlásí časy vydání lišící se o zlomek vteřiny. Vydání porovnávejte podle data.
- `/web/documents-history` podle dokumentace dál vrací zastaralou kategorii `downloaded`. Kde je k dispozici `doc_count`, dejte mu přednost.
- Nezdokumentováno: jednotka časových razítek (`szwm` čte velké hodnoty jako milisekundy), podoba odpovědi 429 a čas obnovení denní kvóty.

## Stav testování

Testy spouštějí celé CLI i MCP server proti lokálnímu falešnému API se zapnutým detektorem souběhů. Mutační kontroly potvrdily, že testy zachytí odstranění maskování klíče, ochrany proti přesměrování, omezení základní URL, režimu jen pro čtení, zkrácení podle kvóty i potvrzování hromadných operací.

Binárka běžela i proti živé službě: veřejné endpointy, MCP handshake přes stdio a chybové cesty pro chybějící a odmítnutý klíč.

**Úspěšná autentizovaná volání zatím proti živému API neproběhla**, protože k tomu je potřeba skutečný klíč. Podoba jejich odpovědí vychází ze zveřejněné specifikace Seznamu, takže pokud narazíte na rozdíl, nahlaste ho.

## Konfigurace

| Proměnná | Účel |
|---|---|
| `SZWM_API_KEY`, `SEZNAM_WEBMASTER_API_KEY` | API klíč |
| `SZWM_PROFILE` | Výchozí profil nebo web |
| `SZWM_OUTPUT` | Výchozí formát výstupu |
| `SZWM_READ_ONLY` | `1` zablokuje všechny zápisy |
| `SZWM_BASE_URL` | Základní URL API: oficiální server nebo loopback |
| `SZWM_CONFIG_DIR` | Adresář s konfigurací |
| `SZWM_STATE_DIR` | Adresář se stavem limitů a kvóty |
| `SZWM_INSECURE_ALLOW_ANY_BASE_URL` | `1` povolí libovolnou základní URL, a tím pošle klíč na daný server |

## Vývoj

```bash
make test    # go test -race ./...
make lint    # kontrola gofmt a go vet
make build   # ./bin/szwm
```

`docs/swagger.snapshot.json` je kontrakt API, proti kterému nástroj vznikl. Agenti pracující na tomto repozitáři by si měli přečíst [AGENTS.md](AGENTS.md).

## Licence

[MIT](LICENSE). Seznam a Seznam.cz jsou ochranné známky společnosti Seznam.cz, a.s., a používají se zde pouze k popisu toho, s čím tento nástroj pracuje.
