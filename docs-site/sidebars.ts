import type {SidebarsConfig} from '@docusaurus/plugin-content-docs';

const sidebars: SidebarsConfig = {
  docsSidebar: [
    'intro',
    {
      type: 'category',
      label: 'Overview',
      collapsed: false,
      items: [
        'overview/what-is-citadel',
        'overview/how-it-works',
        'overview/use-cases',
      ],
    },
    {
      type: 'category',
      label: 'Getting Started',
      collapsed: false,
      items: [
        'getting-started/installation',
        'getting-started/quick-start',
        'getting-started/provisioning',
      ],
    },
    {
      type: 'category',
      label: 'Guides',
      items: [
        'guides/managing-services',
        'guides/monitoring',
        'guides/networking',
        'guides/automation',
      ],
    },
    {
      type: 'category',
      label: 'Architecture',
      items: [
        'architecture/overview',
        'architecture/mesh-network',
        'architecture/job-processing',
        'architecture/status-reporting',
        'architecture/design-decisions',
      ],
    },
    {
      type: 'category',
      label: 'Development',
      items: [
        'development/contributing',
        'development/project-structure',
        'development/adding-handlers',
        'development/testing',
        'development/releasing',
      ],
    },
    {
      type: 'category',
      label: 'Reference',
      items: [
        'reference/commands',
        'reference/configuration',
        'reference/glossary',
      ],
    },
  ],
};

export default sidebars;
