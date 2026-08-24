'use strict';

const fs = require('fs');
const path = require('path');
const root = path.resolve(__dirname, '..', 'web');
const html = fs.readFileSync(path.join(root, 'index.html'), 'utf8');
const script = fs.readFileSync(path.join(root, 'admin.js'), 'utf8');
const attributes = [...html.matchAll(/(?:onclick|onsubmit|onchange)="([^"]+)"/g)]
  .map((match) => match[1])
  .join('\n');
const calls = [...new Set([...attributes.matchAll(/\b([A-Za-z_$][\w$]*)\s*\(/g)].map((match) => match[1]))]
  .filter((name) => !['if', 'confirm'].includes(name));
const missing = calls.filter((name) => !new RegExp(`(?:async\\s+)?function\\s+${name}\\s*\\(`).test(script));

if (missing.length) throw new Error(`missing inline handlers: ${missing.join(', ')}`);
if (/Copilot Bridge|M365 Copilot 网关|>CB</.test(html)) throw new Error('legacy product branding remains');
if ((html.match(/<script src="\/admin\.js"><\/script>/g) || []).length !== 1) throw new Error('admin.js must be loaded exactly once');
if (!html.includes('id="passwordModal"') || !html.includes('id="page-cache"')) throw new Error('required management sections are missing');

console.log(`ui handlers: ${calls.length}; legacy brand: 0; required sections: present`);
