import { defineConfig } from '@rsbuild/core';
import { pluginReact } from '@rsbuild/plugin-react';

const outputRoot = process.env.RELAYOPS_WEB_DIST?.trim() || 'dist';

export default defineConfig({
  plugins: [pluginReact()],
  source: {
    entry: {
      index: './src/main.tsx',
    },
  },
  html: {
    title: '采购与智能定价控制台',
    meta: {
      description: 'NewAPI 上游采购、余额监控与智能定价控制台',
    },
  },
  server: {
    port: 4175,
		proxy: {
			'/api': 'http://127.0.0.1:18180',
		},
  },
  output: {
    distPath: { root: outputRoot },
  },
});
