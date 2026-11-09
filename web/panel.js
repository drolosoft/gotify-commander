// The control panel's script. panel.go inlines it at the end of the page, so
// every element it looks up already exists.
"use strict";

// ─── Constants ──────────────────────────────────────────────────────────────

// The path Gotify mounts the plugin on, without the final slash. Every API
// call is made under it, whether the page was opened with the slash or not.
const basePath = window.location.pathname.replace(/\/+$/, "");

// Where the session, the theme and the password are kept in the browser.
const HISTORY_KEY = "gc_history";
const THEME_KEY = "gc_theme";
const TOKEN_KEY = "gc_token";
const ZOOM_KEY = "gc_zoom";

// How many entries the session keeps. Older ones go as new ones arrive.
const MAX_HISTORY = 30;

// How long a history entry stays in localStorage. Command output can name
// hosts, ports and addresses, so it is kept for a day, not for good.
const HISTORY_TTL_MS = 24 * 60 * 60 * 1000;

// How often the status bar asks the plugin whether it is alive.
const STATUS_INTERVAL_MS = 30000;

// How often the duration of a running command is redrawn.
const RUNNING_TICK_MS = 250;

// How long the copy button shows its tick before it is a copy button again.
const COPIED_FEEDBACK_MS = 1500;

// The theme button goes through these in order. "system" follows the browser.
const THEMES = ["system", "dark", "light"];

// The page sizes the top bar's control steps through, in percent: the same
// ones as Pulse. web/panel.css has a rule for each and web/panel-boot.js
// accepts the same ones from storage.
const ZOOM_STEPS = [50, 60, 70, 80, 90, 100, 110, 125];

// The size of someone who never pressed a button: the browser's own. It has
// no rule in the stylesheet and is never stored.
const ZOOM_DEFAULT = 100;

// From this many services on, the picker gets a box to filter them.
const FILTER_FROM_TARGETS = 9;

// The prompt never lists more suggestions than this; the rest are one more
// typed letter away.
const MAX_SUGGESTIONS = 12;

// A command is suggested by a word of its summary only from this many typed
// letters on; with fewer, almost every summary has a word that matches.
const MIN_SUMMARY_MATCH = 3;

// Asked before running when the configuration could not be loaded and the
// page does not know which commands the plugin calls dangerous.
const CONFIRM_WITHOUT_CONFIG = ["reboot", "shutdown", "docker-restart"];

// The only hosts an answer may show an image from. Drawing an image is a
// request to its host from the user's browser, so it is not made for a host
// that merely appears in the text of an answer.
const IMAGE_HOSTS = ["tile.openstreetmap.org"];

// Where the credit under a map tile points, as the tile licence asks.
const MAP_CREDIT_URL = "https://www.openstreetmap.org/copyright";

// The namespace an <svg> built from the script has to be created in.
const SVG_NAMESPACE = "http://www.w3.org/2000/svg";

// Box-drawing characters and Markdown-like table rows: output that has them
// was laid out in columns and must not wrap.
const COLUMNS_PATTERN = /[─-╿]|^\s*\|.*\|\s*$/m;

// ─── Elements ───────────────────────────────────────────────────────────────

const promptForm = document.getElementById("promptForm");
const promptInput = document.getElementById("prompt");
const runButton = document.getElementById("runButton");
const suggestionList = document.getElementById("suggestions");
const deckTabs = document.getElementById("deckTabs");
const deckPanels = document.getElementById("deckPanels");
const deckNote = document.getElementById("deckNote");
const deckNoteText = document.getElementById("deckNoteText");
const deckRetry = document.getElementById("deckRetry");
const logList = document.getElementById("logList");
const logEmpty = document.getElementById("logEmpty");
const logCount = document.getElementById("logCount");
const foldButton = document.getElementById("foldButton");
const clearButton = document.getElementById("clearButton");
const entryTemplate = document.getElementById("entryTemplate");
const statusBadge = document.getElementById("status");
const statusText = document.getElementById("statusText");
const statusFacts = document.getElementById("statusFacts");
const announcer = document.getElementById("announcer");
const themeButton = document.getElementById("themeButton");
const themeColor = document.getElementById("themeColor");
const lockButton = document.getElementById("lockButton");
const zoomControl = document.getElementById("zoomControl");
const zoomLevel = document.getElementById("zoomLevel");
const authDialog = document.getElementById("authDialog");
const authForm = document.getElementById("authForm");
const authPassword = document.getElementById("authPassword");
const authError = document.getElementById("authError");
const confirmDialog = document.getElementById("confirmDialog");
const confirmCommand = document.getElementById("confirmCommand");
const confirmCancel = document.getElementById("confirmCancel");
const confirmRun = document.getElementById("confirmRun");

// ─── State ──────────────────────────────────────────────────────────────────

// The answer of GET /config: machines, services, categories, commands and
// containers. null until it arrives, and when it could not be loaded.
let panelConfig = null;

// The records of the session by id, as they are stored: what the copy and
// the run-again buttons of an entry read.
const sessionRecords = new Map();

// The picker that is open, as { button, picker }, or null.
let openedPicker = null;

// The suggestions under the prompt and the one the arrow keys are on.
let suggestions = [];
let activeSuggestion = -1;

// Where the up arrow is in the commands already run; -1 is "not recalling".
let recallPosition = -1;

// The command the confirmation dialog is asking about, as
// { commandLine, trigger }, or null. It is only read by the dialog's own
// "Run it" button and written again each time the dialog opens, so a
// cancelled one needs no clearing. Clearing it on the dialog's close event
// was a race: that event arrives late, and could erase the next question.
let pendingRun = null;

// A counter that keeps two entries made in the same millisecond apart.
let entrySequence = 0;

// ─── Small helpers ──────────────────────────────────────────────────────────

/**
 * Creates an element with a class and a text.
 * @param {string} tagName
 * @param {string} [className]
 * @param {string} [text]
 * @returns {HTMLElement}
 */
function element(tagName, className, text) {
  const created = document.createElement(tagName);

  if (className) {
    created.className = className;
  }
  if (text !== undefined) {
    created.textContent = text;
  }

  return created;
}

/**
 * Creates an icon that draws one of the symbols of the page's sprite.
 * @param {string} name - the icon, as in id="icon-<name>"
 * @returns {SVGSVGElement}
 */
function createIcon(name) {
  const icon = document.createElementNS(SVG_NAMESPACE, "svg");
  const use = document.createElementNS(SVG_NAMESPACE, "use");

  icon.setAttribute("class", "icon");
  icon.setAttribute("aria-hidden", "true");
  use.setAttribute("href", "#icon-" + name);
  icon.append(use);

  return icon;
}

/**
 * Points an icon at another drawing of the sprite.
 * @param {Element} holder - the element that contains the <svg class="icon">
 * @param {string} name - the icon, as in id="icon-<name>"
 */
function showIcon(holder, name) {
  holder.querySelector("use").setAttribute("href", "#icon-" + name);
}

/**
 * Says something to a screen reader without moving the focus.
 * @param {string} message
 */
function announce(message) {
  announcer.textContent = message;
}

/**
 * Reads a key from a storage area. Storage can be blocked (private mode, a
 * strict policy), and the panel has to work without it.
 * @param {Storage} storage - localStorage or sessionStorage
 * @param {string} key
 * @returns {string} the value, or "" when there is none or it cannot be read
 */
function readStored(storage, key) {
  try {
    return storage.getItem(key) || "";
  } catch (error) {
    return "";
  }
}

/**
 * Writes a key to a storage area; removes it when the value is "".
 * @param {Storage} storage
 * @param {string} key
 * @param {string} value
 */
function writeStored(storage, key, value) {
  try {
    if (value === "") {
      storage.removeItem(key);
    } else {
      storage.setItem(key, value);
    }
  } catch (error) {
    // Nothing to do: the page goes on with what it has in memory.
  }
}

/**
 * Escapes a text so it can be part of a regular expression.
 * @param {string} text
 * @returns {string}
 */
function escapePattern(text) {
  return text.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

/**
 * Formats how long a command took.
 * @param {number} milliseconds
 * @returns {string} such as "240 ms", "1.4 s" or "1m 05s"
 */
function formatDuration(milliseconds) {
  if (milliseconds < 1000) {
    return milliseconds + " ms";
  }
  if (milliseconds < 60000) {
    return (milliseconds / 1000).toFixed(1) + " s";
  }

  const minutes = Math.floor(milliseconds / 60000);
  const seconds = Math.floor((milliseconds % 60000) / 1000);

  return minutes + "m " + String(seconds).padStart(2, "0") + "s";
}

/**
 * Shortens the uptime Go prints ("52h13m4.52s") to its two biggest units.
 * @param {string} goDuration
 * @returns {string} such as "2d 4h", "3h 25m", "12m" or "40s"
 */
function formatUptime(goDuration) {
  const hours = Number((/(\d+)h/.exec(goDuration) || [])[1] || 0);
  // The "m" of minutes is not followed by "s"; the one of "ms" is.
  const minutes = Number((/(\d+)m(?!s)/.exec(goDuration) || [])[1] || 0);
  // Seconds have a digit right before the "s"; "ms" and "µs" do not.
  const seconds = Math.floor(Number((/([\d.]+)s/.exec(goDuration) || [])[1] || 0));

  if (hours >= 24) {
    return Math.floor(hours / 24) + "d " + (hours % 24) + "h";
  }
  if (hours > 0) {
    return hours + "h " + minutes + "m";
  }
  if (minutes > 0) {
    return minutes + "m";
  }

  return seconds + "s";
}

// ─── Theme ──────────────────────────────────────────────────────────────────

/**
 * Reads the theme the user chose.
 * @returns {string} one of THEMES; "system" when nothing valid is stored
 */
function storedTheme() {
  const stored = readStored(localStorage, THEME_KEY);

  return THEMES.includes(stored) ? stored : "system";
}

/**
 * Puts a theme on the page and describes it on its button. panel-boot.js has
 * already set it before the first paint; this keeps the button, the browser
 * chrome and a later change in step.
 * @param {string} theme - one of THEMES
 */
function applyTheme(theme) {
  const nextTheme = THEMES[(THEMES.indexOf(theme) + 1) % THEMES.length];
  const label = "Theme: " + theme + ". Switch to " + nextTheme + ".";

  document.documentElement.dataset.theme = theme;
  showIcon(themeButton, "theme-" + theme);
  themeButton.setAttribute("aria-label", label);
  themeButton.title = label;

  // The colour of the browser chrome on a phone follows the page. The custom
  // property cannot be read for this: it holds light-dark(), not a colour.
  themeColor.content = getComputedStyle(document.body).backgroundColor;
}

/** Moves to the next theme of THEMES and remembers it. */
function cycleTheme() {
  const nextTheme = THEMES[(THEMES.indexOf(storedTheme()) + 1) % THEMES.length];

  // "system" is the default, so it is stored as no value at all.
  writeStored(localStorage, THEME_KEY, nextTheme === "system" ? "" : nextTheme);
  applyTheme(nextTheme);
}

// ─── Page size ──────────────────────────────────────────────────────────────

/**
 * The page size on screen, which panel-boot.js or an earlier click put on
 * <html>. Anything that is not one of the steps counts as the default.
 * @returns {number} one of ZOOM_STEPS
 */
function currentZoom() {
  const zoom = Number(document.documentElement.dataset.zoom);

  return ZOOM_STEPS.includes(zoom) ? zoom : ZOOM_DEFAULT;
}

/**
 * Where a button of the control leads from a size: one step in or out,
 * holding at both ends, or back to the default.
 * @param {number} zoom - one of ZOOM_STEPS
 * @param {string} action - "in", "out" or "reset"
 * @returns {number}
 */
function zoomAfter(zoom, action) {
  const position = ZOOM_STEPS.indexOf(zoom);

  if (action === "in") {
    return ZOOM_STEPS[Math.min(position + 1, ZOOM_STEPS.length - 1)];
  }
  if (action === "out") {
    return ZOOM_STEPS[Math.max(position - 1, 0)];
  }

  return ZOOM_DEFAULT;
}

/**
 * Writes a size on the control: the percentage and its name on the middle
 * button, and aria-disabled on the button of an end that has been reached,
 * which keeps its focus.
 * @param {number} zoom
 */
function showZoom(zoom) {
  const ends = { out: ZOOM_STEPS[0], in: ZOOM_STEPS[ZOOM_STEPS.length - 1] };

  zoomLevel.textContent = zoom + "\u00A0%";
  zoomLevel.setAttribute("aria-label", zoom + " %, back to 100 %");

  for (const action of Object.keys(ends)) {
    const button = zoomControl.querySelector('[data-zoom-step="' + action + '"]');

    if (zoom === ends[action]) {
      button.setAttribute("aria-disabled", "true");
    } else {
      button.removeAttribute("aria-disabled");
    }
  }
}

/**
 * Puts a size on <html> and remembers it. The default leaves no attribute
 * and no stored value behind, so the page is then exactly the one of someone
 * who never chose.
 * @param {number} zoom
 */
function applyZoom(zoom) {
  if (zoom === ZOOM_DEFAULT) {
    delete document.documentElement.dataset.zoom;
  } else {
    document.documentElement.dataset.zoom = String(zoom);
  }

  writeStored(localStorage, ZOOM_KEY, zoom === ZOOM_DEFAULT ? "" : String(zoom));
}

/**
 * Takes the step of the button that was clicked in the control. A button at
 * its end is aria-disabled, so its click does nothing.
 * @param {MouseEvent} event
 */
function handleZoomClick(event) {
  // The buttons say their step in data-zoom-step; data-zoom is the size
  // itself, on <html>.
  const button = event.target.closest("[data-zoom-step]");
  if (!button || button.getAttribute("aria-disabled") === "true") {
    return;
  }

  const zoom = zoomAfter(currentZoom(), button.dataset.zoomStep);

  applyZoom(zoom);
  showZoom(zoom);
  announce("Page size " + zoom + " %");
}

// ─── Authentication ─────────────────────────────────────────────────────────

// Every API call sends "Authorization: Bearer <password>". The password is
// the one on the plugin page in Gotify (generated, or web_password from the
// plugin configuration). It lives in sessionStorage so it is forgotten when
// the tab closes; authToken keeps it in memory when storage is blocked.
let authToken = readStored(sessionStorage, TOKEN_KEY);

// True once the menus were asked for and the status polling runs.
let appStarted = false;

/**
 * Keeps the password for this tab.
 * @param {string} token
 */
function storeToken(token) {
  authToken = token;
  writeStored(sessionStorage, TOKEN_KEY, token);
}

/** Forgets the password: the server refused it, or the user locked the panel. */
function forgetToken() {
  authToken = "";
  writeStored(sessionStorage, TOKEN_KEY, "");
}

/**
 * Opens the password dialog, or updates its message if it is already open.
 * @param {string} message - why it is asking again; "" for the first time
 */
function openAuthDialog(message) {
  authError.textContent = message || "";
  authError.hidden = !message;

  if (!authDialog.open) {
    authPassword.value = "";
    authDialog.showModal();
  }

  authPassword.focus();
}

/**
 * Builds the error apiFetch rejects with when there is no valid password.
 * @returns {Error}
 */
function unauthorizedError() {
  const error = new Error("Enter the control panel password to continue.");
  error.unauthorized = true;

  return error;
}

/**
 * fetch() against the panel API with the Bearer password. A 401 means the
 * password is missing, wrong or was changed in Gotify: it is forgotten and
 * the dialog asks again.
 * @param {string} path - API path under the panel, such as "/config"
 * @param {RequestInit} [options]
 * @returns {Promise<Response>} rejects with error.unauthorized on a 401
 */
async function apiFetch(path, options) {
  if (!authToken) {
    openAuthDialog("");
    throw unauthorizedError();
  }

  const request = options || {};
  request.headers = request.headers || {};
  request.headers["Authorization"] = "Bearer " + authToken;

  const response = await fetch(basePath + path, request);

  if (response.status === 401) {
    forgetToken();
    showStatus("locked", "Locked", null);
    openAuthDialog("That password was not accepted. Check it on the plugin page in Gotify.");
    throw unauthorizedError();
  }

  return response;
}

/**
 * Takes the password from the dialog. It is not checked here: the next API
 * call does that, and a 401 brings the dialog back with a message. Trimming
 * helps when it is pasted from the plugin page with a stray space.
 * @param {SubmitEvent} event
 */
function submitPassword(event) {
  event.preventDefault();

  const password = authPassword.value.trim();
  if (!password) {
    openAuthDialog("Enter the password first.");
    return;
  }

  storeToken(password);
  authDialog.close();
  startApp();
}

/** Forgets the password and asks for it again, for a shared screen. */
function lockPanel() {
  forgetToken();
  showStatus("locked", "Locked", null);
  openAuthDialog("");
}

// ─── Configuration ──────────────────────────────────────────────────────────

/**
 * The words the user types for a command: its usage up to the first target.
 * @param {{usage: string}} command - an entry of panelConfig.commands
 * @returns {string} "restart" for "restart <service>", "docker ps" for itself
 */
function typedWords(command) {
  return command.usage.replace(/\s*[<[].*$/, "");
}

/**
 * Finds the catalogue entry a command line is about.
 * @param {string} commandLine - what is about to run, such as "docker logs web"
 * @returns {Object|null} the entry of panelConfig.commands, or null
 */
function findCommand(commandLine) {
  const name = commandNameOf(commandLine);
  const commands = panelConfig ? panelConfig.commands || [] : [];

  return commands.find((command) => command.name === name) || null;
}

/**
 * The registered name of the command in a line. docker is the one command
 * with subcommands: "docker restart web" is registered as docker-restart.
 * @param {string} commandLine
 * @returns {string} lower case; "" for an empty line
 */
function commandNameOf(commandLine) {
  const words = commandLine.trim().toLowerCase().split(/\s+/);

  if (words[0] === "docker" && words.length > 1) {
    return "docker-" + words[1];
  }

  return words[0];
}

/**
 * Says whether the panel asks before running a command line.
 * @param {string} commandLine
 * @returns {boolean}
 */
function needsConfirmation(commandLine) {
  const command = findCommand(commandLine);

  if (command) {
    return command.confirm;
  }

  return CONFIRM_WITHOUT_CONFIG.includes(commandNameOf(commandLine));
}

/**
 * The services of the configuration as a sorted list.
 * @returns {Array<{name: string, description: string, machine: string, port: number, domain: string}>}
 */
function serviceList() {
  const services = panelConfig ? panelConfig.services || {} : {};

  return Object.keys(services).sort().map((name) => ({
    name: name,
    description: services[name].description || "",
    machine: services[name].machine || "unknown",
    port: services[name].port || 0,
    domain: services[name].domain || "",
  }));
}

/**
 * The machines of the configuration, sorted. The server lists the ones that
 * have a service, in no particular order.
 * @returns {Array<string>}
 */
function machineList() {
  const machines = panelConfig ? panelConfig.machines || [] : [];

  return machines.slice().sort();
}

/** Asks for the configuration and draws the deck from it. */
async function loadConfig() {
  deckNote.hidden = true;

  try {
    const response = await apiFetch("/config");
    if (!response.ok) {
      throw new Error("GET /config answered " + response.status);
    }

    panelConfig = await response.json();
    servicePattern = buildServicePattern();

    buildDeck(panelConfig.categories || []);
    redrawOutputs();
  } catch (error) {
    // Without a password the dialog is already asking; that is not a failure
    // of the deck.
    if (error.unauthorized) {
      return;
    }

    deckNoteText.textContent = "The commands could not be loaded. The command line still works.";
    deckNote.hidden = false;
  }
}

// ─── Favicons ───────────────────────────────────────────────────────────────

// Matches the name of any service that has a domain, as a whole word. null
// when no service has one. Built once per configuration.
let servicePattern = null;

/**
 * Builds servicePattern from the configuration.
 * @returns {RegExp|null}
 */
function buildServicePattern() {
  const names = serviceList().filter((service) => service.domain).map((service) => service.name);
  if (names.length === 0) {
    return null;
  }

  // Longest first, so "gitea-runner" is not read as "gitea" plus a tail.
  names.sort((first, second) => second.length - first.length);
  const alternatives = names.map(escapePattern).join("|");

  return new RegExp("(?<![\\w.-])(" + alternatives + ")(?![\\w-])", "g");
}

/**
 * Creates the favicon of a service's site. The image comes from the site
 * itself, and one that fails to load takes itself away.
 * @param {string} domain
 * @param {boolean} inline - true inside a line of output
 * @returns {HTMLImageElement}
 */
function faviconOf(domain, inline) {
  const image = element("img", inline ? "favicon favicon-inline" : "favicon");

  image.alt = "";
  image.width = 16;
  image.height = 16;
  image.referrerPolicy = "no-referrer";
  image.addEventListener("error", () => image.remove());
  image.src = "https://" + domain + "/favicon.ico";

  return image;
}

/**
 * Appends a text to an element, with the favicon of each service it names.
 * @param {Element} parent
 * @param {string} text
 */
function appendWithFavicons(parent, text) {
  if (!servicePattern) {
    parent.append(text);
    return;
  }

  const services = panelConfig.services;
  let position = 0;

  for (const match of text.matchAll(servicePattern)) {
    parent.append(text.slice(position, match.index));
    parent.append(faviconOf(services[match[0]].domain, true));
    parent.append(match[0]);
    position = match.index + match[0].length;
  }

  parent.append(text.slice(position));
}

// ─── Output ─────────────────────────────────────────────────────────────────

/**
 * Draws the answer of a command.
 * @param {{message: string, markdown: boolean}} record
 * @returns {HTMLElement}
 */
function renderOutput(record) {
  if (record.markdown) {
    return renderMarkdown(record.message || "");
  }

  return renderPlain(record.message || "");
}

/**
 * Draws plain output as the command printed it.
 * @param {string} text
 * @returns {HTMLElement}
 */
function renderPlain(text) {
  const output = element("pre", "output");

  if (COLUMNS_PATTERN.test(text)) {
    output.dataset.wide = "";
  }
  // A region that scrolls must be reachable with the keyboard.
  output.tabIndex = 0;
  appendWithFavicons(output, text);

  return output;
}

/**
 * Creates a link that opens in another tab, without telling the other site
 * where it came from.
 * @param {string} text
 * @param {string} address - an https or http URL
 * @returns {HTMLAnchorElement}
 */
function externalLink(text, address) {
  const link = element("a", "", text);

  link.href = address;
  link.target = "_blank";
  link.rel = "noopener noreferrer";

  return link;
}

/**
 * Says whether an image of an answer may be drawn, which makes the browser
 * ask its host for it. Text in an answer can come from outside (an address
 * from the geocoder), so only the hosts of IMAGE_HOSTS are asked.
 * @param {string} address
 * @returns {boolean}
 */
function isTrustedImage(address) {
  try {
    return IMAGE_HOSTS.includes(new URL(address).hostname);
  } catch (error) {
    return false;
  }
}

/**
 * Creates the image of a Markdown ![text](https://...): the map tile at the
 * end of "locate", with the credit its licence asks for. An image from any
 * other host becomes a link, so nothing is requested until it is clicked.
 * @param {string} text - what the image shows, for who cannot see it
 * @param {string} address - an https or http URL
 * @returns {HTMLElement}
 */
function markdownImage(text, address) {
  if (!isTrustedImage(address)) {
    return externalLink(text, address);
  }

  const figure = element("span", "prose-map");
  const image = element("img", "prose-image");
  const credit = element("small", "prose-credit");

  image.alt = text;
  image.loading = "lazy";
  // The tile server refuses a browser that sends no Referer. "origin" sends
  // the scheme and the host only: the path of the panel, which carries the
  // plugin token, stays out, as the page's own no-referrer policy wants.
  image.referrerPolicy = "origin";
  image.addEventListener("error", () => figure.remove());
  image.src = address;

  credit.append("\u00A9 ", externalLink("OpenStreetMap contributors", MAP_CREDIT_URL));
  figure.append(image, credit);

  return figure;
}

/**
 * Appends a line of Markdown to an element: `code`, **bold**,
 * [links](https://...) and ![images](https://...) become elements, the rest
 * stays text. Nothing is ever parsed as HTML, so an answer cannot inject
 * markup.
 * @param {Element} parent
 * @param {string} line
 */
function appendInlineMarkdown(parent, line) {
  // The "!" is part of the link's own alternative: read apart, an image
  // would come out as a "!" followed by a link.
  const inlinePattern = /`([^`]+)`|\*\*([^*]+)\*\*|(!?)\[([^\]]+)\]\((https?:\/\/[^\s)]+)\)/g;
  let position = 0;

  for (const match of line.matchAll(inlinePattern)) {
    appendWithFavicons(parent, line.slice(position, match.index));

    if (match[1] !== undefined) {
      parent.append(element("code", "", match[1]));
    } else if (match[2] !== undefined) {
      parent.append(element("strong", "", match[2]));
    } else if (match[3] === "!") {
      parent.append(markdownImage(match[4], match[5]));
    } else {
      parent.append(externalLink(match[4], match[5]));
    }

    position = match.index + match[0].length;
  }

  appendWithFavicons(parent, line.slice(position));
}

/**
 * Splits a row of a Markdown table into its cells.
 * @param {string} line - such as "| restart | Restart a service |"
 * @returns {Array<string>}
 */
function tableCells(line) {
  return line.trim().replace(/^\||\|$/g, "").split("|").map((cell) => cell.trim());
}

/**
 * Draws the subset of Markdown the plugin answers with: headings, tables,
 * lists, rules and paragraphs. It reads the text line by line and keeps the
 * block it is in (a paragraph, a list or a table) until another one starts.
 * @param {string} text
 * @returns {HTMLElement}
 */
function renderMarkdown(text) {
  const prose = element("div", "prose");
  let paragraph = null;
  let list = null;
  let tableBody = null;

  for (const rawLine of text.split("\n")) {
    const line = rawLine.trim();
    const heading = /^(#{1,6})\s+(.*)$/.exec(line);
    const listItem = /^[-*]\s+(.*)$/.exec(line);
    const isTableRow = line.startsWith("|");

    // Anything that is not one more line of the open block closes it.
    if (!isTableRow) {
      tableBody = null;
    }
    if (!listItem) {
      list = null;
    }
    if (isTableRow || listItem || heading || line === "") {
      paragraph = null;
    }

    if (line === "") {
      continue;
    }

    if (heading) {
      // The entry has its own title, so "#" and "##" start at h3.
      const title = element(heading[1].length <= 2 ? "h3" : "h4");
      appendInlineMarkdown(title, heading[2]);
      prose.append(title);
    } else if (/^(-{3,}|\*{3,})$/.test(line)) {
      prose.append(element("hr"));
    } else if (isTableRow) {
      // The row of dashes under the header carries no cells.
      if (/^\|[\s:|-]+\|?$/.test(line)) {
        continue;
      }

      const isHeader = tableBody === null;
      if (isHeader) {
        const wrapper = element("div", "prose-table");
        const table = element("table");
        tableBody = element("tbody");
        table.append(element("thead"), tableBody);
        wrapper.append(table);
        prose.append(wrapper);
      }

      const row = element("tr");
      for (const cell of tableCells(line)) {
        const cellElement = element(isHeader ? "th" : "td");
        appendInlineMarkdown(cellElement, cell);
        row.append(cellElement);
      }

      if (isHeader) {
        tableBody.previousElementSibling.append(row);
      } else {
        tableBody.append(row);
      }
    } else if (listItem) {
      if (!list) {
        list = element("ul");
        prose.append(list);
      }

      const item = element("li");
      appendInlineMarkdown(item, listItem[1]);
      list.append(item);
    } else {
      // The plugin breaks its lines on purpose (one service per line), so a
      // paragraph keeps them instead of joining them as Markdown would.
      if (paragraph) {
        paragraph.append(element("br"));
      } else {
        paragraph = element("p");
        prose.append(paragraph);
      }

      appendInlineMarkdown(paragraph, line);
    }
  }

  return prose;
}

// ─── History ────────────────────────────────────────────────────────────────

/**
 * Reads the saved history and drops the entries older than HISTORY_TTL_MS.
 * When some expired, the pruned list is written back, so their output does
 * not stay in localStorage until the next command.
 * @returns {Array<Object>} the entries still inside the TTL, newest first
 */
function loadHistory() {
  let entries = [];

  try {
    entries = JSON.parse(readStored(localStorage, HISTORY_KEY) || "[]");
  } catch (error) {
    // What is stored is not JSON any more: start again from nothing.
  }
  if (!Array.isArray(entries)) {
    entries = [];
  }

  const fresh = pruneHistory(entries, Date.now());
  if (fresh.length !== entries.length) {
    saveHistory(fresh);
  }

  return fresh;
}

/**
 * Keeps the entries saved less than HISTORY_TTL_MS before now. An entry
 * without savedAt was stored by a version that did not record it; its age
 * is unknown, so it is dropped too.
 * @param {Array<Object>} entries - history entries as stored
 * @param {number} now - the current time in epoch milliseconds
 * @returns {Array<Object>} the entries still inside the TTL, in order
 */
function pruneHistory(entries, now) {
  return entries.filter((entry) => {
    const savedAt = entry ? entry.savedAt : undefined;

    return typeof savedAt === "number" && now - savedAt < HISTORY_TTL_MS;
  });
}

/**
 * Stores the newest MAX_HISTORY entries.
 * @param {Array<Object>} entries - history entries, newest first
 */
function saveHistory(entries) {
  writeStored(localStorage, HISTORY_KEY, JSON.stringify(entries.slice(0, MAX_HISTORY)));
}

/**
 * Makes the id of a new entry of the session.
 * @returns {string}
 */
function nextEntryId() {
  entrySequence += 1;

  return Date.now().toString(36) + "-" + entrySequence;
}

/**
 * The commands already run, newest first and without repeats: what the up
 * arrow of the prompt walks through.
 * @returns {Array<string>}
 */
function recalledCommands() {
  const commands = [];

  for (const entryElement of logList.children) {
    const record = sessionRecords.get(entryElement.dataset.id);

    if (record && !commands.includes(record.command)) {
      commands.push(record.command);
    }
  }

  return commands;
}

// ─── Session log ────────────────────────────────────────────────────────────

/**
 * Creates the entry of a command from the template, with what is known when
 * it starts: its id, its text and the time.
 * @param {string} entryId
 * @param {string} commandLine
 * @param {number} startedAt - epoch milliseconds
 * @returns {HTMLLIElement}
 */
function createEntry(entryId, commandLine, startedAt) {
  const entryElement = entryTemplate.content.firstElementChild.cloneNode(true);
  const time = entryElement.querySelector(".entry-time");
  const started = new Date(startedAt);

  entryElement.dataset.id = entryId;
  entryElement.querySelector(".entry-command").textContent = commandLine;
  time.dateTime = started.toISOString();
  time.textContent = started.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  time.title = started.toLocaleString();

  return entryElement;
}

/**
 * Fills an entry with the answer of its command.
 * @param {HTMLLIElement} entryElement
 * @param {Object} record - a finished record, as stored in the history
 */
function fillEntry(entryElement, record) {
  const state = entryElement.querySelector(".entry-state");
  const duration = entryElement.querySelector(".entry-duration");

  entryElement.dataset.state = record.error ? "failed" : "done";
  showIcon(state, record.error ? "failed" : "done");
  // The colour of the state is not its only sign: the icon changes and a
  // screen reader gets the word.
  state.setAttribute("role", "img");
  state.setAttribute("aria-label", record.error ? "Failed" : "Done");

  duration.textContent = typeof record.durationMs === "number" ? formatDuration(record.durationMs) : "";
  entryElement.querySelector(".entry-title").textContent = record.title || "";
  entryElement.querySelector(".entry-output").replaceChildren(renderOutput(record));
}

/**
 * Folds or unfolds an entry.
 * @param {HTMLLIElement} entryElement
 * @param {boolean} folded
 */
function foldEntry(entryElement, folded) {
  entryElement.toggleAttribute("data-folded", folded);
  entryElement.querySelector(".entry-toggle").setAttribute("aria-expanded", String(!folded));
}

/**
 * Brings the parts around the list in step with it: the count, the empty
 * state and the two tools, which only make sense with entries to act on.
 */
function refreshLogTools() {
  const entries = logList.children.length;
  const anyUnfolded = logList.querySelector(".entry:not([data-folded])") !== null;

  logEmpty.hidden = entries > 0;
  foldButton.hidden = entries === 0;
  clearButton.hidden = entries === 0;
  logCount.textContent = entries === 0 ? "" : entries + (entries === 1 ? " command" : " commands");

  // One button for both: it offers what would change something.
  foldButton.textContent = anyUnfolded ? "Collapse all" : "Expand all";
}

/** Draws the entries kept from earlier in the day, the newest one unfolded. */
function renderSavedSession() {
  const history = loadHistory();

  for (const record of history) {
    // Entries stored before ids existed get one now.
    record.id = record.id || nextEntryId();
    sessionRecords.set(record.id, record);

    const entryElement = createEntry(record.id, record.command, record.savedAt);
    fillEntry(entryElement, record);
    foldEntry(entryElement, logList.children.length > 0);
    logList.append(entryElement);
  }

  refreshLogTools();
}

/**
 * Draws again the output of every finished entry. The saved session is on
 * screen before the configuration arrives, and only then are the services
 * that get a favicon known.
 */
function redrawOutputs() {
  for (const entryElement of logList.children) {
    const record = sessionRecords.get(entryElement.dataset.id);

    if (record) {
      entryElement.querySelector(".entry-output").replaceChildren(renderOutput(record));
    }
  }
}

/**
 * Puts a finished record at the top of the stored history.
 * @param {Object} record
 */
function rememberRecord(record) {
  const history = loadHistory();

  history.unshift(record);
  saveHistory(history);
  sessionRecords.set(record.id, record);
}

/**
 * Takes a record out of the session and of the stored history.
 * @param {string} entryId
 */
function forgetRecord(entryId) {
  sessionRecords.delete(entryId);
  saveHistory(loadHistory().filter((record) => record.id !== entryId));
}

/**
 * Adds to the session something that failed before any command was sent,
 * such as a position the browser would not give.
 * @param {string} commandLine
 * @param {string} title
 * @param {string} message
 */
function reportFailure(commandLine, title, message) {
  const record = {
    id: nextEntryId(),
    command: commandLine,
    title: title,
    message: message,
    error: true,
    markdown: false,
    savedAt: Date.now(),
  };
  const entryElement = createEntry(record.id, commandLine, record.savedAt);

  fillEntry(entryElement, record);
  logList.prepend(entryElement);
  rememberRecord(record);
  refreshLogTools();
  announce(commandLine + " failed: " + message);
}

/**
 * Copies the output of an entry and shows a tick on the button for a moment.
 * @param {HTMLButtonElement} button - the copy button of the entry
 * @param {Object} record
 */
async function copyOutput(button, record) {
  try {
    await navigator.clipboard.writeText(record.message || record.title || "");
  } catch (error) {
    // The clipboard needs HTTPS and the user's permission.
    announce("The output could not be copied.");
    return;
  }

  showIcon(button, "done");
  announce("Output copied.");
  setTimeout(() => showIcon(button, "copy"), COPIED_FEEDBACK_MS);
}

/**
 * Removes an entry and leaves the focus on its neighbour, so the keyboard
 * does not fall back to the top of the page.
 * @param {HTMLLIElement} entryElement
 */
function removeEntry(entryElement) {
  const neighbour = entryElement.nextElementSibling || entryElement.previousElementSibling;

  forgetRecord(entryElement.dataset.id);
  entryElement.remove();
  refreshLogTools();

  if (neighbour) {
    neighbour.querySelector(".entry-toggle").focus();
  } else {
    promptInput.focus();
  }
}

/**
 * Handles every click inside the session list: the fold toggle of an entry
 * and its three actions.
 * @param {MouseEvent} event
 */
function handleLogClick(event) {
  const button = event.target.closest("button");
  if (!button) {
    return;
  }

  const entryElement = button.closest(".entry");
  const record = sessionRecords.get(entryElement.dataset.id);

  if (button.classList.contains("entry-toggle")) {
    foldEntry(entryElement, !entryElement.hasAttribute("data-folded"));
    refreshLogTools();
    return;
  }

  // The actions need the answer, which a running entry does not have yet.
  if (!record) {
    return;
  }

  if (button.dataset.action === "copy") {
    copyOutput(button, record);
  } else if (button.dataset.action === "again") {
    requestRun(record.command, button);
  } else if (button.dataset.action === "remove") {
    removeEntry(entryElement);
  }
}

/** Folds every entry, or unfolds them all when they are all folded. */
function foldOrUnfoldAll() {
  const anyUnfolded = logList.querySelector(".entry:not([data-folded])") !== null;

  for (const entryElement of logList.children) {
    foldEntry(entryElement, anyUnfolded);
  }

  refreshLogTools();
}

/** Empties the session, on screen and in the browser's storage. */
function clearSession() {
  logList.replaceChildren();
  sessionRecords.clear();
  writeStored(localStorage, HISTORY_KEY, "");
  refreshLogTools();
  announce("Session cleared.");
  promptInput.focus();
}

// ─── Running a command ──────────────────────────────────────────────────────

/**
 * Turns the answer of POST /execute into a record of the session.
 * @param {string} commandLine
 * @param {Object} answer - { title, message, markdown } or { error }
 * @returns {Object}
 */
function recordOf(commandLine, answer) {
  // The API refused the request: too many, too big, plugin disabled.
  if (answer.error) {
    return { command: commandLine, title: "Error", message: answer.error, error: true, markdown: false };
  }

  const title = answer.title || "";

  return {
    command: commandLine,
    title: title,
    message: answer.message || "",
    // The plugin starts the title of every failed command with this mark.
    error: title.startsWith("❌"),
    markdown: answer.markdown === true,
  };
}

/**
 * Marks the control that started a command as busy, or frees it.
 * @param {Element|null} trigger
 * @param {boolean} busy
 */
function markBusy(trigger, busy) {
  if (!trigger) {
    return;
  }

  if (busy) {
    trigger.setAttribute("aria-busy", "true");
  } else {
    trigger.removeAttribute("aria-busy");
  }
}

/**
 * Sends a command to the plugin and shows its answer as the newest entry of
 * the session. The entry is there from the start, with the time running, so
 * a slow command is never a page that seems to do nothing.
 * @param {string} commandLine
 * @param {Element|null} trigger - the control that started it, if any
 */
async function runCommand(commandLine, trigger) {
  const entryId = nextEntryId();
  const startedAt = Date.now();
  const entryElement = createEntry(entryId, commandLine, startedAt);
  const duration = entryElement.querySelector(".entry-duration");
  const reducedMotion = window.matchMedia("(prefers-reduced-motion: reduce)").matches;

  entryElement.dataset.state = "running";
  entryElement.querySelector(".entry-output").textContent = "Running…";
  logList.prepend(entryElement);
  refreshLogTools();
  entryElement.scrollIntoView({ block: "nearest", behavior: reducedMotion ? "auto" : "smooth" });

  markBusy(trigger, true);
  const ticker = setInterval(() => {
    duration.textContent = formatDuration(Date.now() - startedAt);
  }, RUNNING_TICK_MS);

  let record;
  try {
    const response = await apiFetch("/execute", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ command: commandLine }),
    });

    record = recordOf(commandLine, await response.json());
  } catch (error) {
    record = {
      command: commandLine,
      title: error.unauthorized ? "Locked" : "Network error",
      message: error.message || "The request failed.",
      error: true,
      markdown: false,
    };
  }

  clearInterval(ticker);
  markBusy(trigger, false);

  record.id = entryId;
  record.savedAt = startedAt;
  record.durationMs = Date.now() - startedAt;

  // The entry may have been cleared while its command ran; the answer is
  // then dropped with it.
  if (!entryElement.isConnected) {
    return;
  }

  rememberRecord(record);
  fillEntry(entryElement, record);
  announce(commandLine + (record.error ? " failed." : " finished."));

  // The count of the status bar is the plugin's, and it has just changed.
  refreshStatus();
}

/**
 * Runs a command line, after asking when the configuration says to.
 * @param {string} commandLine
 * @param {Element|null} trigger - the control that asked for it
 */
function requestRun(commandLine, trigger) {
  const trimmed = commandLine.trim();
  if (!trimmed) {
    return;
  }

  if (needsConfirmation(trimmed)) {
    pendingRun = { commandLine: trimmed, trigger: trigger };
    confirmCommand.textContent = trimmed;
    confirmDialog.showModal();
    // The safe answer has the focus: Enter by reflex does not reboot.
    confirmCancel.focus();
    return;
  }

  runCommand(trimmed, trigger);
}

/** Runs the command the confirmation dialog was asking about. */
function confirmPendingRun() {
  const confirmed = pendingRun;

  pendingRun = null;
  confirmDialog.close();

  if (confirmed) {
    runCommand(confirmed.commandLine, confirmed.trigger);
  }
}

/**
 * Asks the browser for its position and runs "locate" with it.
 * @param {Element|null} trigger
 */
function runLocate(trigger) {
  if (!navigator.geolocation) {
    reportFailure("locate", "Location", "This browser has no location service.");
    return;
  }

  markBusy(trigger, true);
  announce("Asking the browser for its position.");

  navigator.geolocation.getCurrentPosition(
    (position) => {
      const latitude = position.coords.latitude.toFixed(6);
      const longitude = position.coords.longitude.toFixed(6);

      markBusy(trigger, false);
      runCommand("locate " + latitude + " " + longitude, trigger);
    },
    () => {
      markBusy(trigger, false);
      reportFailure("locate", "Location", "The browser did not give its position. Allow it for this page, or type: locate <lat> <lon>");
    },
    { enableHighAccuracy: true, timeout: 10000 }
  );
}

// ─── Deck ───────────────────────────────────────────────────────────────────

/**
 * Shows one category of the deck. Only a narrow screen hides the others;
 * with room, the stylesheet shows them all and hides the tabs.
 * @param {number} index
 */
function selectCategory(index) {
  const tabs = Array.from(deckTabs.children);
  const panels = Array.from(deckPanels.children);

  closePicker();

  tabs.forEach((tab, position) => {
    const selected = position === index;

    tab.setAttribute("aria-selected", String(selected));
    // Only the selected tab is in the tab order; the arrows move between them.
    tab.tabIndex = selected ? 0 : -1;
    panels[position].toggleAttribute("data-current", selected);
  });
}

/**
 * Moves between the tabs of the deck with the arrow keys, as tabs do.
 * @param {KeyboardEvent} event
 */
function handleTabKeys(event) {
  const tabs = Array.from(deckTabs.children);
  const current = tabs.indexOf(document.activeElement);
  let next = -1;

  if (event.key === "ArrowRight") {
    next = (current + 1) % tabs.length;
  } else if (event.key === "ArrowLeft") {
    next = (current - 1 + tabs.length) % tabs.length;
  } else if (event.key === "Home") {
    next = 0;
  } else if (event.key === "End") {
    next = tabs.length - 1;
  }

  if (current === -1 || next === -1) {
    return;
  }

  event.preventDefault();
  selectCategory(next);
  tabs[next].focus();
  tabs[next].scrollIntoView({ block: "nearest", inline: "nearest" });
}

/**
 * Creates the button of a command of the deck.
 * @param {string} name - the registered name, as the configuration lists it
 * @param {{type: string, danger: boolean}} category
 * @param {HTMLElement} picker - where its targets open
 * @returns {HTMLButtonElement}
 */
function createCommandButton(name, category, picker) {
  const command = findCommand(name.replace("-", " "));
  const words = command ? typedWords(command) : name;
  const button = element("button", "command", words);
  const kind = category.type || "direct";

  button.type = "button";
  button.title = command ? command.summary : "";

  if (command ? command.confirm : category.danger) {
    button.dataset.confirm = "";
  }

  // A command of a "machine" or "service" category opens its targets first.
  if (kind !== "direct") {
    button.setAttribute("aria-expanded", "false");
  }

  button.addEventListener("click", () => {
    if (kind !== "direct") {
      togglePicker(button, picker, words, kind);
    } else if (name === "locate") {
      closePicker();
      runLocate(button);
    } else {
      closePicker();
      requestRun(words, button);
    }
  });

  return button;
}

/**
 * Draws the deck: a tab and a panel per category of the configuration.
 * @param {Array<{label: string, type: string, commands: Array<string>, danger: boolean}>} categories
 */
function buildDeck(categories) {
  openedPicker = null;
  deckTabs.replaceChildren();
  deckPanels.replaceChildren();

  categories.forEach((category, index) => {
    const tab = element("button", "deck-tab", category.label);
    const panel = element("div", "deck-panel");
    const grid = element("div", "deck-grid");
    const picker = element("div", "picker");

    tab.type = "button";
    tab.id = "deckTab" + index;
    tab.setAttribute("role", "tab");
    tab.setAttribute("aria-controls", "deckPanel" + index);
    tab.addEventListener("click", () => selectCategory(index));

    panel.id = "deckPanel" + index;
    panel.setAttribute("role", "tabpanel");
    panel.setAttribute("aria-labelledby", tab.id);
    picker.hidden = true;

    for (const name of category.commands || []) {
      grid.append(createCommandButton(name, category, picker));
    }

    panel.append(element("h2", "micro-label deck-title", category.label), grid, picker);
    deckTabs.append(tab);
    deckPanels.append(panel);
  });

  if (categories.length > 0) {
    selectCategory(0);
  }
}

// ─── Target picker ──────────────────────────────────────────────────────────

/**
 * Creates the button of one target of the picker.
 * @param {string} words - the command, such as "restart"
 * @param {{name: string, note: string, domain: string}} target
 * @param {HTMLButtonElement} commandButton - the command of the deck
 * @returns {HTMLButtonElement}
 */
function createTargetButton(words, target, commandButton) {
  const button = element("button", "target");
  const text = element("span", "target-text");

  button.type = "button";
  button.dataset.name = target.name;

  if (commandButton.hasAttribute("data-confirm")) {
    button.dataset.confirm = "";
  }
  if (target.domain) {
    button.append(faviconOf(target.domain, false));
  }

  text.append(element("span", "target-name", target.name));
  if (target.note) {
    text.append(element("span", "target-note", target.note));
  }
  button.append(text);

  button.addEventListener("click", () => {
    closePicker();
    requestRun(words + " " + target.name, commandButton);
  });

  return button;
}

/**
 * The targets a picker offers, in groups.
 * @param {string} kind - "machine" or "service", the type of the category
 * @returns {Array<{title: string, targets: Array<Object>}>}
 */
function targetGroups(kind) {
  if (kind === "machine") {
    const machines = machineList().map((machine) => ({ name: machine, note: "", domain: "" }));

    return machines.length > 0 ? [{ title: "", targets: machines }] : [];
  }

  // Services go under their machine, which is how one thinks of them.
  const groups = new Map();
  for (const service of serviceList()) {
    const port = service.port ? ":" + service.port : "";
    const note = [service.description, port].filter(Boolean).join(" · ");

    if (!groups.has(service.machine)) {
      groups.set(service.machine, []);
    }
    groups.get(service.machine).push({ name: service.name, note: note, domain: service.domain });
  }

  return Array.from(groups.keys()).sort().map((machine) => ({ title: machine, targets: groups.get(machine) }));
}

/**
 * Hides the targets of a picker that do not match what is typed in its
 * filter, and the groups left with none.
 * @param {HTMLElement} picker
 * @param {string} typed
 */
function filterTargets(picker, typed) {
  const wanted = typed.trim().toLowerCase();

  for (const group of picker.querySelectorAll(".picker-group")) {
    let visible = 0;

    for (const button of group.querySelectorAll(".target")) {
      const matches = button.textContent.toLowerCase().includes(wanted);

      button.hidden = !matches;
      visible += matches ? 1 : 0;
    }

    group.hidden = visible === 0;
  }
}

/**
 * Fills a picker with the targets of a command.
 * @param {HTMLElement} picker
 * @param {string} words - the command, such as "restart"
 * @param {string} kind - "machine" or "service"
 * @param {HTMLButtonElement} commandButton
 */
function fillPicker(picker, words, kind, commandButton) {
  const groups = targetGroups(kind);
  const total = groups.reduce((count, group) => count + group.targets.length, 0);
  const head = element("div", "picker-head");
  const title = element("p", "picker-title");
  const close = element("button", "icon-button icon-button-small");

  title.append(element("code", "", words), kind === "machine" ? " on which machine?" : " for which service?");
  close.type = "button";
  close.setAttribute("aria-label", "Close");
  close.append(createIcon("remove"));
  close.addEventListener("click", () => {
    closePicker();
    commandButton.focus();
  });
  head.append(title, close);
  picker.replaceChildren(head);

  if (total === 0) {
    picker.append(element("p", "picker-empty", "The configuration has no " + kind + "s yet."));
    return;
  }

  if (total >= FILTER_FROM_TARGETS) {
    const filter = element("input", "field picker-filter");

    filter.type = "search";
    filter.placeholder = "Filter";
    filter.setAttribute("aria-label", "Filter the " + kind + "s");
    filter.addEventListener("input", () => filterTargets(picker, filter.value));
    picker.append(filter);
  }

  for (const group of groups) {
    const groupElement = element("div", "picker-group");
    const options = element("div", "picker-options");

    if (group.title) {
      groupElement.append(element("h3", "micro-label picker-group-title", group.title + " · " + group.targets.length));
    }
    for (const target of group.targets) {
      options.append(createTargetButton(words, target, commandButton));
    }

    groupElement.append(options);
    picker.append(groupElement);
  }
}

/** Closes the picker that is open, if any. */
function closePicker() {
  if (!openedPicker) {
    return;
  }

  openedPicker.button.setAttribute("aria-expanded", "false");
  openedPicker.picker.hidden = true;
  openedPicker.picker.replaceChildren();
  openedPicker = null;
}

/**
 * Opens the targets of a command under its grid, or closes them when they
 * are the ones already open.
 * @param {HTMLButtonElement} button - the command of the deck
 * @param {HTMLElement} picker
 * @param {string} words
 * @param {string} kind
 */
function togglePicker(button, picker, words, kind) {
  const wasOpen = openedPicker !== null && openedPicker.button === button;

  closePicker();
  if (wasOpen) {
    return;
  }

  fillPicker(picker, words, kind, button);
  picker.hidden = false;
  button.setAttribute("aria-expanded", "true");
  openedPicker = { button: button, picker: picker };

  // The focus goes where the next choice is: the filter, or the first target.
  const first = picker.querySelector(".picker-filter, .target");
  if (first) {
    first.focus();
  }
}

// ─── Prompt ─────────────────────────────────────────────────────────────────

/**
 * Says whether any word of a text starts with what is typed.
 * @param {string} text
 * @param {string} typed - lower case
 * @returns {boolean}
 */
function hasWordStarting(text, typed) {
  return text.toLowerCase().split(/[\s-]+/).some((word) => word.startsWith(typed));
}

/**
 * Puts the names that start with what is typed before the ones that only
 * contain it, and drops the rest.
 * @param {Array<Object>} candidates
 * @param {string} typed - lower case
 * @param {function(Object): string} nameOf
 * @returns {Array<Object>}
 */
function rankByMatch(candidates, typed, nameOf) {
  const starting = candidates.filter((candidate) => nameOf(candidate).startsWith(typed));
  const containing = candidates.filter((candidate) => !nameOf(candidate).startsWith(typed) && nameOf(candidate).includes(typed));

  return starting.concat(containing);
}

/**
 * The commands to suggest for the first letters of a line, best first: the
 * ones whose words start with them ("re" gives restart), then the ones with
 * a later word that does ("re" gives docker restart), then, from three
 * letters on, the ones whose summary has such a word ("disk" gives df).
 * Letters in the middle of a word do not count: "re" is not "free".
 * @param {Array<Object>} commands - panelConfig.commands
 * @param {string} typed - lower case
 * @returns {Array<Object>}
 */
function matchingCommands(commands, typed) {
  const starting = commands.filter((command) => typedWords(command).startsWith(typed));
  const laterWord = commands.filter((command) => !starting.includes(command) && hasWordStarting(typedWords(command), typed));
  const found = starting.concat(laterWord);

  if (typed.length < MIN_SUMMARY_MATCH) {
    return found;
  }

  const described = commands.filter((command) => !found.includes(command) && hasWordStarting(command.summary, typed));

  return found.concat(described);
}

/**
 * The targets a command can take, as suggestions without the command.
 * @param {{target: string}} command
 * @returns {Array<{name: string, note: string}>}
 */
function targetsOf(command) {
  if (command.target === "machine") {
    return machineList().map((machine) => ({ name: machine, note: "machine" }));
  }
  if (command.target === "service") {
    return serviceList().map((service) => ({ name: service.name, note: [service.description, service.machine].filter(Boolean).join(" · ") }));
  }
  if (command.target === "container") {
    return (panelConfig.containers || []).map((container) => ({ name: container, note: "container" }));
  }

  return [];
}

/**
 * Works out what to suggest for what is typed in the prompt: commands while
 * the first words are being typed, then the targets of the command.
 * @param {string} text - the value of the prompt
 * @returns {Array<{text: string, label: string, note: string, confirm: boolean, complete: boolean}>}
 *   text is what accepting it types; complete says it can run as it is
 */
function suggestionsFor(text) {
  const typed = text.trimStart().toLowerCase();
  const commands = panelConfig ? panelConfig.commands || [] : [];

  if (typed === "") {
    return [];
  }

  // The longest command whose words are already typed in full, if any:
  // "docker restart " is docker-restart, not docker.
  const typedCommands = commands.filter((command) => (typed + " ").startsWith(typedWords(command) + " "));
  typedCommands.sort((first, second) => typedWords(second).length - typedWords(first).length);
  const command = typedCommands[0];

  // A command without targets only counts while nothing follows it:
  // "docker re" is the start of "docker restart", not docker with a target.
  const awaitsTarget = command && (command.target !== "" || typed === typedWords(command));

  if (awaitsTarget) {
    const words = typedWords(command);
    const partial = typed.slice(words.length).trimStart();

    // A second word is already typed in full: nothing more to offer.
    if (partial.includes(" ")) {
      return [];
    }

    const targets = rankByMatch(targetsOf(command), partial, (target) => target.name.toLowerCase());
    const found = targets.map((target) => ({
      text: words + " " + target.name,
      label: words + " " + target.name,
      note: target.note,
      confirm: command.confirm,
      complete: true,
    }));

    // The target is exactly one of them: the line is finished.
    if (found.length === 1 && found[0].text === typed) {
      return [];
    }

    // Typed without a space yet, the command itself is still an answer,
    // and so are the commands that start with the same letters.
    if (typed === words) {
      const longer = commands.filter((other) => other !== command && typedWords(other).startsWith(typed));
      const offered = [command].concat(longer).map(commandSuggestion).concat(found);

      // Only itself to offer: the line is finished and Enter runs it.
      return offered.length === 1 ? [] : offered.slice(0, MAX_SUGGESTIONS);
    }

    return found.slice(0, MAX_SUGGESTIONS);
  }

  return matchingCommands(commands, typed).map(commandSuggestion).slice(0, MAX_SUGGESTIONS);
}

/**
 * Turns a command of the catalogue into a suggestion of the prompt.
 * @param {Object} command - an entry of panelConfig.commands
 * @returns {{text: string, label: string, note: string, confirm: boolean, complete: boolean}}
 */
function commandSuggestion(command) {
  return {
    text: typedWords(command),
    label: command.usage,
    note: command.summary,
    confirm: command.confirm,
    // A command with a required target is not ready until it has one.
    complete: !command.required,
  };
}

/** Closes the list of suggestions. */
function closeSuggestions() {
  suggestions = [];
  activeSuggestion = -1;
  suggestionList.hidden = true;
  suggestionList.replaceChildren();
  promptInput.setAttribute("aria-expanded", "false");
  promptInput.removeAttribute("aria-activedescendant");
}

/** Draws the suggestions for what is typed, or closes the list without any. */
function refreshSuggestions() {
  suggestions = suggestionsFor(promptInput.value);
  activeSuggestion = -1;

  if (suggestions.length === 0) {
    closeSuggestions();
    return;
  }

  const options = suggestions.map((suggestion, index) => {
    const option = element("li", "suggestion");

    option.id = "suggestion" + index;
    option.setAttribute("role", "option");
    option.setAttribute("aria-selected", "false");
    option.dataset.index = String(index);
    if (suggestion.confirm) {
      option.dataset.confirm = "";
    }
    option.append(element("span", "suggestion-text", suggestion.label), element("span", "suggestion-note", suggestion.note));

    return option;
  });

  suggestionList.replaceChildren(...options);
  suggestionList.hidden = false;
  promptInput.setAttribute("aria-expanded", "true");
  promptInput.removeAttribute("aria-activedescendant");
}

/**
 * Moves the highlighted suggestion with the arrow keys, around the ends.
 * @param {number} step - 1 for down, -1 for up
 */
function moveSuggestion(step) {
  const options = suggestionList.children;

  if (activeSuggestion >= 0) {
    options[activeSuggestion].setAttribute("aria-selected", "false");
  }

  // From "none" (-1), down goes to the first and up goes to the last.
  if (activeSuggestion === -1 && step < 0) {
    activeSuggestion = options.length - 1;
  } else {
    activeSuggestion = (activeSuggestion + step + options.length) % options.length;
  }

  options[activeSuggestion].setAttribute("aria-selected", "true");
  options[activeSuggestion].scrollIntoView({ block: "nearest" });
  promptInput.setAttribute("aria-activedescendant", options[activeSuggestion].id);
}

/**
 * Types a suggestion into the prompt. A finished line can run at once; one
 * that still needs a target gets a space and the list of its targets.
 * @param {number} index - position in suggestions
 * @param {boolean} run - true to run it when it is complete
 */
function acceptSuggestion(index, run) {
  const suggestion = suggestions[index];

  if (suggestion.complete && run) {
    promptInput.value = suggestion.text;
    submitPrompt();
    return;
  }

  // A space after a command that can take a target brings its targets up.
  const takesMore = suggestion.label !== suggestion.text;
  promptInput.value = suggestion.text + (takesMore ? " " : "");
  refreshSuggestions();
}

/**
 * Walks through the commands already run, as a shell does.
 * @param {number} step - 1 for older, -1 for newer
 */
function recallCommand(step) {
  const commands = recalledCommands();
  const position = Math.min(recallPosition + step, commands.length - 1);

  recallPosition = Math.max(position, -1);
  promptInput.value = recallPosition === -1 ? "" : commands[recallPosition];
}

/** Runs what is typed in the prompt and empties it for the next command. */
function submitPrompt() {
  const commandLine = promptInput.value.trim();

  closeSuggestions();
  closePicker();
  recallPosition = -1;
  promptInput.value = "";

  // "locate" alone means "where am I": the browser gives the position.
  if (commandLine.toLowerCase() === "locate") {
    runLocate(runButton);
    return;
  }

  requestRun(commandLine, runButton);
}

/**
 * The keys of the prompt: arrows walk the suggestions, or the commands
 * already run when there are none; Tab completes; Enter accepts and runs;
 * Escape closes the list.
 * @param {KeyboardEvent} event
 */
function handlePromptKeys(event) {
  const listOpen = suggestions.length > 0;
  // Recalling only starts from an empty prompt, so the arrows never throw
  // away something half typed.
  const canRecall = promptInput.value === "" || recallPosition !== -1;

  if (event.key === "ArrowDown" || event.key === "ArrowUp") {
    const step = event.key === "ArrowDown" ? 1 : -1;

    if (listOpen) {
      event.preventDefault();
      moveSuggestion(step);
    } else if (canRecall) {
      event.preventDefault();
      // Up is "older", which is further into the list.
      recallCommand(-step);
    }
  } else if (event.key === "Tab" && listOpen && !event.shiftKey) {
    event.preventDefault();
    acceptSuggestion(Math.max(activeSuggestion, 0), false);
  } else if (event.key === "Enter" && listOpen && activeSuggestion >= 0) {
    event.preventDefault();
    acceptSuggestion(activeSuggestion, true);
  } else if (event.key === "Escape" && listOpen) {
    event.preventDefault();
    closeSuggestions();
  }
}

/**
 * The shortcuts of the page: "/" and Ctrl+K (or Cmd+K) go to the prompt, and
 * Escape closes the open picker.
 * @param {KeyboardEvent} event
 */
function handlePageKeys(event) {
  const target = event.target;
  const typing = target instanceof HTMLInputElement || target instanceof HTMLTextAreaElement;
  const dialogOpen = authDialog.open || confirmDialog.open;
  const paletteKey = (event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "k";

  if (dialogOpen) {
    return;
  }

  if (paletteKey || (event.key === "/" && !typing)) {
    event.preventDefault();
    promptInput.focus();
    promptInput.select();
  } else if (event.key === "Escape" && openedPicker && target !== promptInput) {
    const commandButton = openedPicker.button;

    closePicker();
    commandButton.focus();
  }
}

// ─── Status bar ─────────────────────────────────────────────────────────────

/**
 * Draws the status bar.
 * @param {string} state - "connected", "disconnected", "disabled" or "locked"
 * @param {string} text - the word for it
 * @param {Object|null} health - the answer of GET /health, when there is one
 */
function showStatus(state, text, health) {
  statusBadge.dataset.state = state;
  statusText.textContent = text;
  statusFacts.replaceChildren();

  if (!health) {
    return;
  }

  const executed = health.commands_executed || 0;
  const facts = {
    uptime: "up " + formatUptime(health.uptime || ""),
    commands: executed + (executed === 1 ? " command run" : " commands run"),
    // A release is "v0.4.0" already; a local build says "dev".
    version: health.version || "",
  };

  for (const name of Object.keys(facts)) {
    const fact = element("span", "", facts[name]);

    fact.dataset.fact = name;
    statusFacts.append(fact);
  }
}

/** Asks the plugin how it is and draws the answer in the status bar. */
async function refreshStatus() {
  try {
    const response = await apiFetch("/health");

    if (response.status === 503) {
      showStatus("disabled", "Plugin disabled", null);
      return;
    }
    if (!response.ok) {
      throw new Error("GET /health answered " + response.status);
    }

    showStatus("connected", "Connected", await response.json());
  } catch (error) {
    if (error.unauthorized) {
      showStatus("locked", "Locked", null);
    } else {
      showStatus("disconnected", "Disconnected", null);
    }
  }
}

// ─── Start ──────────────────────────────────────────────────────────────────

/** Loads the deck and starts the status polling once. */
function startApp() {
  loadConfig();
  refreshStatus();

  if (!appStarted) {
    appStarted = true;
    setInterval(refreshStatus, STATUS_INTERVAL_MS);
  }
}

themeButton.addEventListener("click", cycleTheme);
lockButton.addEventListener("click", lockPanel);
zoomControl.addEventListener("click", handleZoomClick);
authForm.addEventListener("submit", submitPassword);
deckRetry.addEventListener("click", loadConfig);
deckTabs.addEventListener("keydown", handleTabKeys);
logList.addEventListener("click", handleLogClick);
foldButton.addEventListener("click", foldOrUnfoldAll);
clearButton.addEventListener("click", clearSession);
confirmRun.addEventListener("click", confirmPendingRun);
confirmCancel.addEventListener("click", () => confirmDialog.close());
document.addEventListener("keydown", handlePageKeys);

// Escape would close the password dialog and leave a panel that cannot do
// anything.
authDialog.addEventListener("cancel", (event) => event.preventDefault());

promptForm.addEventListener("submit", (event) => {
  event.preventDefault();
  submitPrompt();
});
promptInput.addEventListener("keydown", handlePromptKeys);
promptInput.addEventListener("input", () => {
  recallPosition = -1;
  refreshSuggestions();
});

// Leaving the command line closes its list. A click on a suggestion does not
// leave it: mousedown is cancelled below, so the prompt keeps the focus.
promptForm.addEventListener("focusout", (event) => {
  if (!promptForm.contains(event.relatedTarget)) {
    closeSuggestions();
  }
});
suggestionList.addEventListener("mousedown", (event) => event.preventDefault());
suggestionList.addEventListener("click", (event) => {
  const option = event.target.closest(".suggestion");

  if (option) {
    acceptSuggestion(Number(option.dataset.index), true);
    promptInput.focus();
  }
});

// In "system" the page follows the browser by itself; the button and the
// browser chrome are told here.
window.matchMedia("(prefers-color-scheme: dark)").addEventListener("change", () => applyTheme(storedTheme()));

applyTheme(storedTheme());
// The markup says 100 %; when panel-boot.js painted another size, the number
// and the ends are corrected here, without announcing anything.
showZoom(currentZoom());
renderSavedSession();

if (authToken) {
  startApp();
} else {
  showStatus("locked", "Locked", null);
  openAuthDialog("");
}
