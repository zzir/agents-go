import { useState } from 'react';

const GREETINGS: [number, string, string[]][] = [
  [6,  '🌌', ['Thoughts sharpen in the silence', 'The world sleeps, the mind wakes', 'Stillness is the seed of clarity']],
  [12, '🌅', ['A blank canvas, infinite paths', 'Begin before the doubts arrive', 'Morning knows what evening forgot']],
  [18, '☀️', ['Midway through, the view clears', 'Momentum hides in plain sight', 'The obstacle is the material']],
  [22, '🌇', ['The best work outlives the day', 'Dusk trades effort for insight', 'What you built today echoes tomorrow']],
];
const GREETING_NIGHT: [string, string[]] = ['🌙', ['One more thought before the stars', 'Night falls, ideas rise', 'The dark is just depth in disguise']];

function getGreeting(): [string, string] {
  const h = new Date().getHours();
  let emoji: string;
  let pool: string[];
  let matched = false;
  for (const [until, e, texts] of GREETINGS) {
    if (h < until) { emoji = e; pool = texts; matched = true; break; }
  }
  if (!matched) { emoji = GREETING_NIGHT[0]; pool = GREETING_NIGHT[1]; }
  return [emoji!, pool![Math.floor(Math.random() * pool!.length)]];
}

// The empty session's slogan.
export function Greeting() {
  // Picked once per mount; the call site keys this by session id, so the slogan
  // holds across composer re-renders and rerolls only when another session opens.
  const [[emoji, text]] = useState(getGreeting);
  return (
    <div className="chat-greeting">
      <span className="chat-greeting-emoji">{emoji}</span>
      <span className="chat-greeting-text">{text}</span>
    </div>
  );
}
