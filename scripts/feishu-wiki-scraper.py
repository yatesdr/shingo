#!/usr/bin/env python3
"""
Feishu Wiki Scraper — crawls a Feishu wiki tree and saves each page as Markdown.

Usage:
    pip install playwright markdownify
    playwright install chromium
    python feishu-wiki-scraper.py "https://seer-group.feishu.cn/wiki/QdCIwrnFkiTeirkqrnTcF8gln6g" -o ./seer-rds-docs

The script opens a visible browser so you can log in if needed, then crawls
the wiki sidebar tree and saves each page.
"""

import argparse
import json
import re
import time
from pathlib import Path
from urllib.parse import urlparse

from markdownify import markdownify as md
from playwright.sync_api import sync_playwright


def slugify(text: str) -> str:
    """Turn a page title into a safe filename."""
    text = re.sub(r'[^\w\s-]', '', text.strip())
    text = re.sub(r'[\s]+', '-', text)
    return text[:120] or 'untitled'


def wait_for_content(page, timeout=20):
    """Wait for the page content to stabilize (stop changing)."""
    page.wait_for_load_state("networkidle", timeout=30000)
    # Poll body text length until it stabilizes
    prev_len = 0
    stable_count = 0
    deadline = time.time() + timeout
    while time.time() < deadline:
        cur_len = page.evaluate("() => document.body.innerText.length")
        if cur_len > 100 and cur_len == prev_len:
            stable_count += 1
            if stable_count >= 3:
                return
        else:
            stable_count = 0
        prev_len = cur_len
        time.sleep(0.5)


def debug_dump(page, output_dir: Path):
    """Save a screenshot and DOM summary for debugging selectors."""
    output_dir.mkdir(parents=True, exist_ok=True)
    page.screenshot(path=str(output_dir / "debug-screenshot.png"), full_page=True)

    # Dump top-level DOM structure with class names
    structure = page.evaluate('''() => {
        function describe(el, depth) {
            if (depth > 4) return [];
            const lines = [];
            for (const child of el.children) {
                const tag = child.tagName.toLowerCase();
                const cls = child.className && typeof child.className === 'string'
                    ? '.' + child.className.split(/\\s+/).filter(Boolean).join('.')
                    : '';
                const id = child.id ? '#' + child.id : '';
                const textLen = child.innerText ? child.innerText.length : 0;
                const indent = '  '.repeat(depth);
                lines.push(`${indent}<${tag}${id}${cls}> text=${textLen}`);
                lines.push(...describe(child, depth + 1));
            }
            return lines;
        }
        return describe(document.body, 0).join('\\n');
    }''')
    (output_dir / "debug-dom-structure.txt").write_text(structure, encoding="utf-8")
    print(f"  Debug files saved to {output_dir}/debug-*")


def extract_page_content(page) -> tuple[str, str]:
    """Extract title and markdown content from the current Feishu wiki page."""
    wait_for_content(page)

    title = page.title()
    # Try to get a cleaner title from the page
    try:
        heading = page.evaluate('''() => {
            // Try various heading selectors
            const selectors = ['h1', '[data-page-title]', '[class*="title"]'];
            for (const sel of selectors) {
                const el = document.querySelector(sel);
                if (el && el.innerText.trim().length > 0 && el.innerText.trim().length < 200) {
                    return el.innerText.trim();
                }
            }
            return '';
        }''')
        if heading:
            title = heading
    except Exception:
        pass

    # Use JavaScript to find the largest content block on the page,
    # excluding obvious navigation/sidebar elements
    content_html = page.evaluate('''() => {
        // Strategy: find the element with the most text content that looks like
        // a main content area (not nav, sidebar, header, footer)
        const excludeTags = new Set(['NAV', 'HEADER', 'FOOTER', 'SCRIPT', 'STYLE', 'NOSCRIPT']);
        const excludeClassPatterns = [/sidebar/i, /catalog/i, /nav/i, /header/i, /footer/i, /menu/i, /toolbar/i, /topbar/i];

        function shouldExclude(el) {
            if (excludeTags.has(el.tagName)) return true;
            const cls = (el.className && typeof el.className === 'string') ? el.className : '';
            return excludeClassPatterns.some(p => p.test(cls));
        }

        // First, try to find a known content container
        const knownSelectors = [
            '[data-content-editable-root]',
            '[class*="docx-content"]',
            '[class*="doc-content"]',
            '[class*="wiki-content"]',
            '[class*="page-content"]',
            '[class*="render"]',
            '[class*="editor"]',
            'article',
            'main',
            '[role="main"]',
            '[role="article"]',
        ];
        for (const sel of knownSelectors) {
            const el = document.querySelector(sel);
            if (el && el.innerText.trim().length > 50) {
                return el.innerHTML;
            }
        }

        // Fallback: walk the DOM and find the deepest element with the most text
        let best = null;
        let bestScore = 0;
        function walk(el) {
            if (shouldExclude(el)) return;
            const text = el.innerText || '';
            const textLen = text.length;
            // Prefer elements that are deep enough (not body) but have lots of text
            if (textLen > bestScore && el !== document.body && el.children.length > 0) {
                best = el;
                bestScore = textLen;
            }
            for (const child of el.children) {
                walk(child);
            }
        }
        walk(document.body);

        if (best) {
            // Clone and strip navigation elements
            const clone = best.cloneNode(true);
            clone.querySelectorAll('nav, header, footer, [class*="sidebar"], [class*="catalog"], [class*="menu"], [class*="toolbar"]')
                .forEach(e => e.remove());
            return clone.innerHTML;
        }
        return document.body.innerHTML;
    }''')

    markdown = md(content_html, heading_style="ATX", strip=['script', 'style', 'svg'])
    # Clean up excessive blank lines
    markdown = re.sub(r'\n{3,}', '\n\n', markdown)
    return title, markdown


def discover_wiki_links(page, base_url: str) -> list[dict]:
    """Find child wiki page links from the sidebar/catalog tree and inline links."""
    links = page.evaluate('''(baseHost) => {
        const results = [];
        const seen = new Set();
        // Grab ALL links on the page that point to wiki pages on the same host
        document.querySelectorAll('a[href*="/wiki/"]').forEach(a => {
            const href = a.href;
            const text = a.innerText.trim();
            if (href && href.includes('/wiki/') && !seen.has(href)) {
                try {
                    const url = new URL(href);
                    if (url.host === baseHost && text.length > 0 && text.length < 200) {
                        // Normalize: strip query params and hash
                        const clean = url.origin + url.pathname;
                        if (!seen.has(clean)) {
                            seen.add(clean);
                            results.push({url: clean, title: text});
                        }
                    }
                } catch(e) {}
            }
        });
        return results;
    }''', urlparse(base_url).netloc)
    return links


def expand_sidebar_tree(page):
    """Try to expand all collapsed tree nodes in the wiki sidebar."""
    for attempt in range(15):
        clicked = page.evaluate('''() => {
            let clicked = 0;
            // Look for any collapsed/expandable toggle elements
            const toggleSelectors = [
                '[class*="expand"]',
                '[class*="arrow"]',
                '[class*="toggle"]',
                '[class*="collapsed"]',
                '[class*="fold"]',
                '[aria-expanded="false"]',
            ];
            for (const sel of toggleSelectors) {
                document.querySelectorAll(sel).forEach(el => {
                    try {
                        // Only click small elements (likely toggle icons, not content)
                        const rect = el.getBoundingClientRect();
                        if (rect.width > 0 && rect.width < 50 && rect.height > 0 && rect.height < 50) {
                            // Only click elements in the left portion of the page (sidebar area)
                            if (rect.left < 400) {
                                el.click();
                                clicked++;
                            }
                        }
                    } catch(e) {}
                });
            }
            return clicked;
        }''')
        if clicked == 0:
            break
        print(f"    expanded {clicked} nodes (pass {attempt + 1})...")
        time.sleep(1)


def main():
    parser = argparse.ArgumentParser(description="Scrape a Feishu wiki to Markdown files")
    parser.add_argument("url", help="Starting Feishu wiki URL")
    parser.add_argument("-o", "--output", default="./seer-rds-docs", help="Output directory")
    parser.add_argument("--no-crawl", action="store_true", help="Only scrape the single page, don't follow sidebar links")
    parser.add_argument("--debug", action="store_true", help="Save screenshot and DOM dump for debugging")
    args = parser.parse_args()

    output_dir = Path(args.output)
    output_dir.mkdir(parents=True, exist_ok=True)

    with sync_playwright() as p:
        # Launch visible browser so user can log in if needed
        browser = p.chromium.launch(headless=False)
        context = browser.new_context(viewport={"width": 1400, "height": 900})
        page = context.new_page()

        print(f"Navigating to {args.url} ...")
        page.goto(args.url, wait_until="networkidle", timeout=60000)

        # Check if we got redirected to login
        if "accounts" in page.url and "login" in page.url:
            print("\n*** Login required! Please log in via the browser window. ***")
            print("*** Waiting for you to complete login and reach the wiki page... ***\n")
            page.wait_for_url("**/wiki/**", timeout=300000)  # wait up to 5 minutes
            page.wait_for_load_state("networkidle")
            time.sleep(3)

        if args.debug:
            print("Debug mode: dumping page structure...")
            debug_dump(page, output_dir)
            print("Check debug-screenshot.png and debug-dom-structure.txt")
            print("You can re-run without --debug once selectors are confirmed.")
            browser.close()
            return

        # Save the starting page
        print("Extracting starting page...")
        title, content = extract_page_content(page)
        filename = f"00-{slugify(title)}.md"
        (output_dir / filename).write_text(f"# {title}\n\n{content}", encoding="utf-8")
        print(f"  Saved: {filename}")

        if args.no_crawl:
            print("Done (--no-crawl).")
            browser.close()
            return

        # Expand sidebar tree and discover all wiki links
        print("Expanding sidebar tree...")
        expand_sidebar_tree(page)
        time.sleep(1)

        print("Discovering wiki pages...")
        links = discover_wiki_links(page, args.url)
        # Remove the current page from the list
        start_id = args.url.rstrip('/').split('/')[-1]
        links = [l for l in links if start_id not in l['url']]

        print(f"Found {len(links)} linked wiki pages.")

        for i, link in enumerate(links, 1):
            url = link['url']
            link_title = link['title']
            print(f"  [{i}/{len(links)}] {link_title[:60]}...")

            try:
                page.goto(url, wait_until="networkidle", timeout=30000)
                title, content = extract_page_content(page)
                filename = f"{i:02d}-{slugify(title)}.md"
                (output_dir / filename).write_text(f"# {title}\n\n{content}", encoding="utf-8")
                print(f"           Saved: {filename}")
            except Exception as e:
                print(f"           ERROR: {e}")

        print(f"\nDone! {len(links) + 1} pages saved to {output_dir}")
        browser.close()


if __name__ == "__main__":
    main()
