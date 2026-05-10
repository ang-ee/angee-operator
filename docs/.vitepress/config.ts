import { defineConfig } from 'vitepress'

export default defineConfig({
  title: 'Angee',
  description: 'Self-managed stack manager for agent-native applications.',
  cleanUrls: true,
  lastUpdated: true,
  srcExclude: ['scripts/**', 'README.md'],
  sitemap: { hostname: 'https://docs.angee.ai' },
  head: [
    ['link', { rel: 'icon', href: '/favicon.svg', type: 'image/svg+xml' }],
    ['meta', { name: 'theme-color', content: '#5B6CFF' }],
  ],
  themeConfig: {
    logo: '/logo.svg',
    siteTitle: 'Angee',
    nav: [
      { text: 'Guide', link: '/guide/getting-started' },
      { text: 'Reference', link: '/reference/operator-api' },
      {
        text: 'v0.4.7',
        items: [
          { text: 'Changelog', link: 'https://github.com/fyltr/angee/blob/main/CHANGELOG.md' },
          { text: 'Releases', link: 'https://github.com/fyltr/angee/releases' },
        ],
      },
    ],
    sidebar: {
      '/guide/': [
        {
          text: 'Guide',
          items: [
            { text: 'Getting started', link: '/guide/getting-started' },
            { text: 'Concepts', link: '/guide/concepts' },
            { text: 'Commands', link: '/guide/commands' },
            { text: 'Manifest', link: '/guide/manifest' },
            { text: 'Templates', link: '/guide/templates' },
            { text: 'Development', link: '/guide/development' },
          ],
        },
      ],
      '/reference/': [
        {
          text: 'Reference',
          items: [
            { text: 'Operator API', link: '/reference/operator-api' },
            { text: 'GraphQL schema', link: '/reference/graphql/' },
            { text: 'Manifest schema', link: '/reference/manifest-schema' },
            { text: 'Surface parity', link: '/reference/surfaces' },
          ],
        },
      ],
    },
    socialLinks: [
      { icon: 'github', link: 'https://github.com/fyltr/angee' },
    ],
    search: { provider: 'local' },
    editLink: {
      pattern: 'https://github.com/fyltr/angee/edit/main/docs/:path',
      text: 'Edit this page on GitHub',
    },
    footer: {
      message: 'Released under the AGPL-3.0 License.',
      copyright: 'Copyright © 2025-present Fyltr',
    },
  },
})
