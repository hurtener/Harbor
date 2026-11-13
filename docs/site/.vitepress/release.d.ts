// Injected by config.ts (vite `define`) at build time.
declare const __HARBOR_RELEASE__: {
  version: string;
  short: string;
  date: string;
  summary: string;
  eyebrow: string;
  announcement: string;
  recent: {
    version: string;
    date: string;
    summary: string;
    items: { title: string; body: string }[];
  }[];
};

declare const __HARBOR_FACTS__: {
  protocolMethods: number;
  cliCommands: number;
  operatorSkills: number;
  recipes: number;
};
