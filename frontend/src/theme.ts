export const THEME_STORAGE_KEY = "llama-lab-theme";

export const THEME_OPTIONS = [
  "Dark Matter",
  "Light Matter",
  "Tropics",
  "Tropic Night",
  "Ocean",
  "Coffee",
  "Cliffs",
  "Cyber Punk",
  "Neon Purple",
  "Space",
  "Sky",
  "Forest",
  "Sun"
] as const;

export type ThemeName = (typeof THEME_OPTIONS)[number];

type ThemeVars = {
  bg: string;
  panel: string;
  ink: string;
  inkStrong: string;
  accent: string;
  accentSoft: string;
  line: string;
  glow: string;
  sidebarStart: string;
  sidebarEnd: string;
  newEmailBorder: string;
  newEmailStart: string;
  newEmailEnd: string;
  newEmailText: string;
  buttonText: string;
  linkBorder: string;
};

const themes: Record<ThemeName, ThemeVars> = {
  "Dark Matter": {
    bg: "#1a1a1e",
    panel: "#252530",
    ink: "#d4c5e2",
    inkStrong: "#e8ddf5",
    accent: "#c29a72",
    accentSoft: "#5a3f31",
    line: "#404050",
    glow: "rgba(107, 74, 66, 0.25)",
    sidebarStart: "#1f1f24",
    sidebarEnd: "#2a2530",
    newEmailBorder: "#8f6b4a",
    newEmailStart: "#c29a72",
    newEmailEnd: "#9a7450",
    newEmailText: "#24170f",
    buttonText: "#24170f",
    linkBorder: "#d8dde6"
  },
  "Light Matter": {
    bg: "#f5efe5",
    panel: "#fff8ee",
    ink: "#4c3d32",
    inkStrong: "#2d1f15",
    accent: "#c29a72",
    accentSoft: "#e6d2be",
    line: "#c5b29d",
    glow: "rgba(175, 126, 92, 0.2)",
    sidebarStart: "#ede2d2",
    sidebarEnd: "#e4d6c3",
    newEmailBorder: "#8f6b4a",
    newEmailStart: "#c29a72",
    newEmailEnd: "#9a7450",
    newEmailText: "#24170f",
    buttonText: "#24170f",
    linkBorder: "#b7a38c"
  },
  Tropics: {
    bg: "#f4f1eb",
    panel: "#fffaf0",
    ink: "#43362d",
    inkStrong: "#241a14",
    accent: "#9bc400",
    accentSoft: "#d4e3a0",
    line: "#c4b7a3",
    glow: "rgba(123, 165, 31, 0.2)",
    sidebarStart: "#ece5d8",
    sidebarEnd: "#e3dacb",
    newEmailBorder: "#78a100",
    newEmailStart: "#9bc400",
    newEmailEnd: "#7ea100",
    newEmailText: "#243100",
    buttonText: "#243100",
    linkBorder: "#bcb0a0"
  },
  "Tropic Night": {
    bg: "#15131a",
    panel: "#221f2b",
    ink: "#cdbde0",
    inkStrong: "#e8ddf5",
    accent: "#9bc400",
    accentSoft: "#6b4a42",
    line: "#3c3650",
    glow: "rgba(107, 74, 66, 0.28)",
    sidebarStart: "#1d1a24",
    sidebarEnd: "#292233",
    newEmailBorder: "#78a100",
    newEmailStart: "#9bc400",
    newEmailEnd: "#7ea100",
    newEmailText: "#1a2400",
    buttonText: "#1a2400",
    linkBorder: "#7f7599"
  },
  Ocean: {
    bg: "#0f1b24",
    panel: "#152a36",
    ink: "#b8d8e8",
    inkStrong: "#e0f2fb",
    accent: "#5ea9be",
    accentSoft: "#214657",
    line: "#2f5567",
    glow: "rgba(58, 130, 155, 0.24)",
    sidebarStart: "#112430",
    sidebarEnd: "#173342",
    newEmailBorder: "#4f91a6",
    newEmailStart: "#74bacd",
    newEmailEnd: "#4f91a6",
    newEmailText: "#0a1b22",
    buttonText: "#0a1b22",
    linkBorder: "#7ba7b8"
  },
  Coffee: {
    bg: "#1d1714",
    panel: "#2a211d",
    ink: "#d6c0b3",
    inkStrong: "#f0ded2",
    accent: "#b47f5c",
    accentSoft: "#5f3f2f",
    line: "#4a3830",
    glow: "rgba(132, 86, 61, 0.24)",
    sidebarStart: "#231a16",
    sidebarEnd: "#32251f",
    newEmailBorder: "#8f5f42",
    newEmailStart: "#b47f5c",
    newEmailEnd: "#8f5f42",
    newEmailText: "#220f08",
    buttonText: "#220f08",
    linkBorder: "#8f7a6d"
  },
  Cliffs: {
    bg: "#241c18",
    panel: "#302723",
    ink: "#d8c7ba",
    inkStrong: "#f2e5db",
    accent: "#c78f6e",
    accentSoft: "#6b4637",
    line: "#56443c",
    glow: "rgba(176, 121, 87, 0.24)",
    sidebarStart: "#2a201c",
    sidebarEnd: "#3a2b25",
    newEmailBorder: "#9d6e55",
    newEmailStart: "#c78f6e",
    newEmailEnd: "#9d6e55",
    newEmailText: "#27140c",
    buttonText: "#27140c",
    linkBorder: "#a9958a"
  },
  "Cyber Punk": {
    bg: "#120918",
    panel: "#1e1028",
    ink: "#f5d0ff",
    inkStrong: "#ffe9ff",
    accent: "#00f5d4",
    accentSoft: "#3b1760",
    line: "#5c2d84",
    glow: "rgba(255, 0, 153, 0.2)",
    sidebarStart: "#1b0d24",
    sidebarEnd: "#281236",
    newEmailBorder: "#00c9ad",
    newEmailStart: "#00f5d4",
    newEmailEnd: "#00c9ad",
    newEmailText: "#051d1a",
    buttonText: "#051d1a",
    linkBorder: "#c38fdd"
  },
  "Neon Purple": {
    bg: "#130b1d",
    panel: "#231233",
    ink: "#e4ccff",
    inkStrong: "#f2e6ff",
    accent: "#c86cff",
    accentSoft: "#47206c",
    line: "#63358a",
    glow: "rgba(200, 108, 255, 0.2)",
    sidebarStart: "#1b1029",
    sidebarEnd: "#2a1740",
    newEmailBorder: "#9d45d3",
    newEmailStart: "#c86cff",
    newEmailEnd: "#9d45d3",
    newEmailText: "#210a35",
    buttonText: "#210a35",
    linkBorder: "#b78ed9"
  },
  Space: {
    bg: "#0b0f1a",
    panel: "#151c2d",
    ink: "#c8d5f0",
    inkStrong: "#e7efff",
    accent: "#86a8ff",
    accentSoft: "#263e74",
    line: "#34496f",
    glow: "rgba(92, 126, 220, 0.18)",
    sidebarStart: "#0f1625",
    sidebarEnd: "#18233a",
    newEmailBorder: "#6788dd",
    newEmailStart: "#86a8ff",
    newEmailEnd: "#6788dd",
    newEmailText: "#101930",
    buttonText: "#101930",
    linkBorder: "#8ca0c8"
  },
  Sky: {
    bg: "#dff1ff",
    panel: "#f4fbff",
    ink: "#2f4f64",
    inkStrong: "#183142",
    accent: "#6db3d6",
    accentSoft: "#b6dced",
    line: "#93bdd2",
    glow: "rgba(109, 179, 214, 0.28)",
    sidebarStart: "#d3ecfa",
    sidebarEnd: "#c2e2f4",
    newEmailBorder: "#4f93b8",
    newEmailStart: "#6db3d6",
    newEmailEnd: "#4f93b8",
    newEmailText: "#0f2e3f",
    buttonText: "#0f2e3f",
    linkBorder: "#89afc2"
  },
  Forest: {
    bg: "#142018",
    panel: "#1f2f24",
    ink: "#c7dbc7",
    inkStrong: "#e3f0df",
    accent: "#8faa74",
    accentSoft: "#3a5837",
    line: "#4f694f",
    glow: "rgba(118, 148, 95, 0.24)",
    sidebarStart: "#18261c",
    sidebarEnd: "#223629",
    newEmailBorder: "#6f8d5a",
    newEmailStart: "#8faa74",
    newEmailEnd: "#6f8d5a",
    newEmailText: "#12200f",
    buttonText: "#12200f",
    linkBorder: "#90a98d"
  },
  Sun: {
    bg: "#fff3dc",
    panel: "#fff9ec",
    ink: "#5a4024",
    inkStrong: "#392611",
    accent: "#e0ab4f",
    accentSoft: "#f1d9a2",
    line: "#d4b27a",
    glow: "rgba(224, 171, 79, 0.28)",
    sidebarStart: "#f8e7c5",
    sidebarEnd: "#f2dab1",
    newEmailBorder: "#bb8631",
    newEmailStart: "#e0ab4f",
    newEmailEnd: "#bb8631",
    newEmailText: "#2a1808",
    buttonText: "#2a1808",
    linkBorder: "#caa670"
  }
};

function isThemeName(value: string): value is ThemeName {
  return THEME_OPTIONS.includes(value as ThemeName);
}

function applyThemeVars(theme: ThemeVars) {
  const root = document.documentElement;
  root.style.setProperty("--bg", theme.bg);
  root.style.setProperty("--panel", theme.panel);
  root.style.setProperty("--ink", theme.ink);
  root.style.setProperty("--ink-strong", theme.inkStrong);
  root.style.setProperty("--accent", theme.accent);
  root.style.setProperty("--accent-soft", theme.accentSoft);
  root.style.setProperty("--line", theme.line);
  root.style.setProperty("--glow", theme.glow);
  root.style.setProperty("--sidebar-start", theme.sidebarStart);
  root.style.setProperty("--sidebar-end", theme.sidebarEnd);
  root.style.setProperty("--new-email-border", theme.newEmailBorder);
  root.style.setProperty("--new-email-start", theme.newEmailStart);
  root.style.setProperty("--new-email-end", theme.newEmailEnd);
  root.style.setProperty("--new-email-text", theme.newEmailText);
  root.style.setProperty("--button-text", theme.buttonText);
  root.style.setProperty("--link-border", theme.linkBorder);
}

export function applyTheme(themeName: ThemeName) {
  applyThemeVars(themes[themeName]);
  try {
    window.localStorage.setItem(THEME_STORAGE_KEY, themeName);
  } catch {
    // Ignore unavailable storage.
  }
}

export function getStoredTheme(): ThemeName {
  try {
    const saved = window.localStorage.getItem(THEME_STORAGE_KEY) ?? "Dark Matter";
    if (isThemeName(saved)) {
      return saved;
    }
    if (saved === "Current") {
      return "Dark Matter";
    }
    if (saved === "Old Light") {
      return "Tropics";
    }
    if (saved === "Old Dark") {
      return "Tropic Night";
    }
    return "Dark Matter";
  } catch {
    return "Dark Matter";
  }
}

export function applyStoredTheme() {
  const theme = getStoredTheme();
  applyThemeVars(themes[theme]);
}
