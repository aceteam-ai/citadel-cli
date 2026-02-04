import {themes as prismThemes} from 'prism-react-renderer';
import type {Config} from '@docusaurus/types';
import type * as Preset from '@docusaurus/preset-classic';

const config: Config = {
  title: 'Citadel CLI',
  tagline: 'Connect your hardware to the AceTeam Sovereign Compute Fabric',
  favicon: 'img/favicon.ico',

  future: {
    v4: true,
  },

  url: 'https://aceteam-ai.github.io',
  baseUrl: '/citadel-cli/',

  organizationName: 'aceteam-ai',
  projectName: 'citadel-cli',

  onBrokenLinks: 'throw',

  markdown: {
    mermaid: true,
  },

  themes: ['@docusaurus/theme-mermaid'],

  i18n: {
    defaultLocale: 'en',
    locales: ['en'],
  },

  presets: [
    [
      'classic',
      {
        docs: {
          sidebarPath: './sidebars.ts',
          routeBasePath: '/',
          editUrl:
            'https://github.com/aceteam-ai/citadel-cli/tree/main/docs-site/',
        },
        blog: false,
        theme: {
          customCss: './src/css/custom.css',
        },
      } satisfies Preset.Options,
    ],
  ],

  themeConfig: {
    colorMode: {
      respectPrefersColorScheme: true,
    },
    navbar: {
      title: 'Citadel CLI',
      items: [
        {
          type: 'docSidebar',
          sidebarId: 'docsSidebar',
          position: 'left',
          label: 'Docs',
        },
        {
          href: 'https://aceteam.ai',
          label: 'AceTeam',
          position: 'right',
        },
        {
          href: 'https://github.com/aceteam-ai/citadel-cli',
          label: 'GitHub',
          position: 'right',
        },
      ],
    },
    footer: {
      style: 'dark',
      links: [
        {
          title: 'Documentation',
          items: [
            {label: 'Getting Started', to: '/getting-started/quick-start'},
            {label: 'Architecture', to: '/architecture/overview'},
            {label: 'Command Reference', to: '/reference/commands'},
          ],
        },
        {
          title: 'AceTeam',
          items: [
            {label: 'Platform', href: 'https://aceteam.ai'},
            {label: 'GitHub', href: 'https://github.com/aceteam-ai'},
          ],
        },
      ],
      copyright: `Copyright © ${new Date().getFullYear()} AceTeam.ai. Built with Docusaurus.`,
    },
    prism: {
      theme: prismThemes.github,
      darkTheme: prismThemes.dracula,
      additionalLanguages: ['bash', 'go', 'yaml', 'json', 'powershell'],
    },
  } satisfies Preset.ThemeConfig,
};

export default config;
