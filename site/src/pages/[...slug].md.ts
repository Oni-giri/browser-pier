// Every docs page as plain Markdown, at its own URL plus ".md". Pages are
// written as .md (site/STYLE.md), so the source is the page.
import type { APIRoute, GetStaticPaths } from 'astro';
import { getCollection, type CollectionEntry } from 'astro:content';

export const getStaticPaths: GetStaticPaths = async () => {
	const pages = await getCollection('docs');
	return pages.map((entry) => ({ params: { slug: entry.id }, props: { entry } }));
};

export const GET: APIRoute<{ entry: CollectionEntry<'docs'> }> = ({ props }) => {
	const { title, description } = props.entry.data;
	// The description as a quote, as llms.txt does: BrowserPier's read_docs
	// (internal/docs) reads it back from there.
	const head = `# ${title}\n\n${description ? `> ${description}\n\n` : ''}`;
	return new Response(head + (props.entry.body ?? '').trim() + '\n', {
		headers: { 'Content-Type': 'text/markdown; charset=utf-8' },
	});
};
