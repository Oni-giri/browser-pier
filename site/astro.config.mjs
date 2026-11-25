// @ts-check
import { fileURLToPath } from 'node:url';
import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';
import starlightLlmsTxt from 'starlight-llms-txt';

const description =
	'BrowserPier runs one real, signed-in Chromium per profile on your own server, for AI agents to drive over MCP, REST or CDP, and hands the browser to you when a step needs a person.';

export default defineConfig({
	site: 'https://browserpier.com',
	// The docs live under /docs; the landing page will own the root.
	base: '/docs',
	vite: {
		// The fonts and the grain come from the web UI's own files
		// (internal/ui/static), so there's one copy of each.
		server: { fs: { allow: [fileURLToPath(new URL('..', import.meta.url))] } },
	},
	integrations: [
		starlight({
			title: 'BrowserPier',
			description,
			logo: {
				light: './src/assets/logo-light.svg',
				dark: './src/assets/logo-dark.svg',
				alt: '',
			},
			favicon: '/favicon.svg',
			social: [{ icon: 'github', label: 'GitHub', href: 'https://github.com/Oni-giri/browser-pier' }],
			editLink: { baseUrl: 'https://github.com/Oni-giri/browser-pier/edit/main/site/' },
			customCss: ['./src/styles/theme.css'],
			components: {
				SiteTitle: './src/components/SiteTitle.astro',
				PageTitle: './src/components/PageTitle.astro',
			},
			// Code blocks are dark cards in both themes, as on the landing page.
			expressiveCode: {
				themes: ['github-dark-default'],
				useStarlightDarkModeSwitch: false,
				useStarlightUiThemeColors: false,
				// No empty terminal title bars; a block with a title still gets one.
				defaultProps: { frame: 'code' },
				styleOverrides: {
					borderRadius: '12px',
					borderColor: 'transparent',
					codeBackground: '#0F172A',
					codeFontFamily: '"Geist Mono", ui-monospace, SFMono-Regular, Menlo, monospace',
					codeFontSize: '13px',
					codeLineHeight: '1.7',
					uiFontFamily: 'Geist, system-ui, sans-serif',
					frames: {
						editorBackground: '#0F172A',
						terminalBackground: '#0F172A',
						editorTabBarBackground: '#0F172A',
						terminalTitlebarBackground: '#0F172A',
						editorActiveTabBackground: '#0F172A',
						editorActiveTabIndicatorBottomColor: 'transparent',
						editorTabBarBorderBottomColor: 'rgba(148,163,184,.14)',
						terminalTitlebarBorderBottomColor: 'rgba(148,163,184,.14)',
						frameBoxShadowCssValue: 'none',
						tooltipSuccessBackground: '#16A34A',
					},
				},
			},
			sidebar: [
				{ label: 'Overview', link: '/' },
				{ label: 'Guides', items: ['guides/handoff'] },
				{ label: 'Reference', items: ['reference/sessions', 'reference/handoffs'] },
			],
			plugins: [
				starlightLlmsTxt({
					projectName: 'BrowserPier',
					description,
					details:
						'Every page is also served as Markdown: add `.md` to its URL, for example `https://browserpier.com/docs/reference/sessions.md`. Examples use `https://pier.example.com` for the BrowserPier instance and read the agent token from `$PIER_TOKEN`.',
				}),
			],
		}),
	],
});
