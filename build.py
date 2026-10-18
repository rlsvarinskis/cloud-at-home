"""Builds attic-cloud.svg (the README illustration of the main branch, docs/assets/attic-cloud.svg).

Usage: python3 build.py   (needs only the Python standard library; logos are embedded as data URIs)
"""
import base64, pathlib
R = pathlib.Path(__file__).resolve().parent  # logos/ is next to this script; the SVG is written here too
def uri(path, mime):
    return f"data:{mime};base64," + base64.b64encode((R/path).read_bytes()).decode()
AWS = uri("logos/Amazon_Web_Services_Logo.svg", "image/svg+xml")
GCP = uri("logos/Google_Cloud_icon_(2026).svg", "image/svg+xml")
OAI = uri("logos/OpenAI_logo_2025_(symbol).svg", "image/svg+xml")
ANT = uri("logos/anthropic.png", "image/png")

def plaque(cx, y, w, h, href, iw, ih):
    return (f'<rect x="{cx-w/2}" y="{y}" width="{w}" height="{h}" rx="9" fill="#fff" stroke="#1b2a42" stroke-opacity=".3" stroke-width="1.5"/>'
            f'<image href="{href}" x="{cx-iw/2}" y="{y+(h-ih)/2}" width="{iw}" height="{ih}" preserveAspectRatio="xMidYMid meet"/>')

svg = f'''<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 800 640" role="img" aria-labelledby="t d">
  <title id="t">Attic Cloud</title>
  <desc id="d">A big cloud full of square-jawed giants wearing cloud-provider logos hovers over mom's house. In the attic a sad guy sits among hastily assembled server racks.</desc>
  <defs>
    <linearGradient id="sky" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="#3b5b8f"/><stop offset="1" stop-color="#bcd7ee"/></linearGradient>
    <linearGradient id="cloud" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="#ffffff"/><stop offset="1" stop-color="#cfd8e3"/></linearGradient>
    <radialGradient id="glow" cx="400" cy="318" r="230" gradientUnits="userSpaceOnUse"><stop offset="0" stop-color="#ffe9a8"/><stop offset=".5" stop-color="#b98a52"/><stop offset="1" stop-color="#4b3220"/></radialGradient>
    <clipPath id="atticClip"><path d="M200 450 L400 280 L600 450 Z"/></clipPath>
    <symbol id="chad" viewBox="0 0 100 130">
      <path d="M2 130 Q6 86 50 82 Q94 86 98 130 Z" fill="#d79a6b"/>
      <rect x="38" y="66" width="24" height="22" fill="#cf8f60"/>
      <path d="M27 20 Q50 4 73 20 L78 52 L70 76 L30 76 L22 52 Z" fill="#e8b58a"/>
      <path d="M26 24 Q50 0 74 24 L74 30 Q50 14 26 30 Z" fill="#2a1d14"/>
      <rect x="28" y="36" width="44" height="10" rx="3" fill="#111"/>
      <path d="M40 62 Q50 67 60 62" stroke="#8c4f2e" stroke-width="2.5" fill="none" stroke-linecap="round"/>
      <path d="M50 70 L50 74" stroke="#cf8f60" stroke-width="2"/>
    </symbol>
    <marker id="arr" markerWidth="10" markerHeight="10" refX="8" refY="5" orient="auto"><path d="M0 0 L10 5 L0 10 Z" fill="#10203a"/></marker>
  </defs>

  <rect width="800" height="640" fill="url(#sky)"/>

  <!-- the big, far-away cloud -->
  <g fill="url(#cloud)">
    <circle cx="150" cy="150" r="70"/><circle cx="250" cy="105" r="85"/><circle cx="380" cy="90" r="95"/>
    <circle cx="520" cy="105" r="85"/><circle cx="640" cy="150" r="70"/>
    <rect x="130" y="130" width="530" height="90" rx="45"/>
  </g>
  <use href="#chad" x="110" y="70" width="100" height="130"/>
  <use href="#chad" x="215" y="40" width="120" height="156"/>
  <use href="#chad" x="345" y="20" width="140" height="182"/>
  <use href="#chad" x="490" y="40" width="120" height="156"/>
  <use href="#chad" x="600" y="70" width="100" height="130"/>
  <!-- money, everywhere (it is the expensive kind of cloud) -->
  <g font-family="Georgia, serif" font-weight="bold" text-anchor="middle" fill="#22b24c" stroke="#0e6b2c" stroke-width="1.2" paint-order="stroke">
    <text x="95" y="120" font-size="30" transform="rotate(-12 95 120)">$</text>
    <text x="190" y="62" font-size="24" transform="rotate(10 190 62)">$</text>
    <text x="140" y="205" font-size="20" transform="rotate(8 140 205)">$</text>
    <text x="235" y="36" font-size="20" transform="rotate(-6 235 36)">$</text>
    <text x="330" y="66" font-size="28" transform="rotate(14 330 66)">$</text>
    <text x="300" y="205" font-size="22" transform="rotate(-10 300 205)">$</text>
    <text x="372" y="30" font-size="22" transform="rotate(6 372 30)">$</text>
    <text x="455" y="60" font-size="26" transform="rotate(-8 455 60)">$</text>
    <text x="505" y="28" font-size="20" transform="rotate(12 505 28)">$</text>
    <text x="590" y="52" font-size="30" transform="rotate(9 590 52)">$</text>
    <text x="682" y="112" font-size="28" transform="rotate(-14 682 112)">$</text>
    <text x="700" y="178" font-size="22" transform="rotate(10 700 178)">$</text>
    <text x="500" y="205" font-size="24" transform="rotate(-9 500 205)">$</text>
    <text x="620" y="208" font-size="20" transform="rotate(6 620 208)">$</text>
    <text x="115" y="160" font-size="18" transform="rotate(-8 115 160)">$</text>
    <text x="80" y="185" font-size="22" transform="rotate(12 80 185)">$</text>
    <text x="352" y="214" font-size="18" transform="rotate(-6 352 214)">$</text>
    <text x="690" y="70" font-size="20" transform="rotate(8 690 70)">$</text>
    <text x="205" y="205" font-size="24" transform="rotate(14 205 205)">$</text>
    <text x="430" y="214" font-size="20" transform="rotate(-12 430 214)">$</text>
    <text x="565" y="210" font-size="18" transform="rotate(8 565 210)">$</text>
    <text x="260" y="190" font-size="16" transform="rotate(-5 260 190)">$</text>
    <text x="660" y="40" font-size="18" transform="rotate(-10 660 40)">$</text>
    <text x="150" y="40" font-size="22" transform="rotate(12 150 40)">$</text>
  </g>
  <!-- provider logos on their chests (artwork supplied by the repo owner) -->
  {plaque(160, 158, 62, 38, AWS, 50, 30)}
  {plaque(275, 150, 62, 44, GCP, 44, 36)}
  {plaque(415, 142, 70, 50, ANT, 40, 40)}
  {plaque(550, 150, 62, 44, OAI, 34, 34)}
  <g font-family="Helvetica, Arial, sans-serif" font-weight="bold" font-size="14" text-anchor="middle">
    <rect x="618" y="158" width="64" height="30" rx="9" fill="#0078d4" stroke="#1b2a42" stroke-opacity=".3" stroke-width="1.5"/>
    <text x="650" y="178" fill="#fff">Azure</text>
  </g>
  <path d="M120 204 Q400 236 690 204 L690 218 Q400 254 120 218 Z" fill="#cfd8e3"/>
  <text x="400" y="248" text-anchor="middle" font-family="Georgia, serif" font-size="17" fill="#2b3a52" font-style="italic">The Cloud (very expensive, very far away)</text>

  <!-- ground -->
  <rect x="0" y="575" width="800" height="65" fill="#6aa354"/><rect x="0" y="575" width="800" height="8" fill="#588f45"/>

  <!-- mom's house: roof, cut away to show the attic -->
  <!-- chimney first, so the roof overlaps its base -->
  <rect x="498" y="288" width="30" height="96" fill="#8f3f33" stroke="#6e2c22" stroke-width="3"/>
  <rect x="493" y="281" width="40" height="10" fill="#7a3328" stroke="#6e2c22" stroke-width="3"/>
  <path d="M165 458 L400 252 L635 458 Z" fill="#a8483a" stroke="#6e2c22" stroke-width="4" stroke-linejoin="round"/>
  <path d="M200 450 L400 280 L600 450 Z" fill="url(#glow)"/>
  <g clip-path="url(#atticClip)">
    <!-- rafters -->
    <g stroke="#3a2616" stroke-width="3" opacity=".55"><path d="M400 280 L330 450"/><path d="M400 280 L470 450"/><path d="M400 262 L400 450" stroke-width="2"/></g>
    <!-- bare bulb -->
    <path d="M400 281 L400 304" stroke="#222" stroke-width="2"/><circle cx="400" cy="309" r="6" fill="#fff6c9" stroke="#c9a74a" stroke-width="1.5"/>

    <!-- floor cables, tangled -->
    <g fill="none" stroke-width="3" stroke-linecap="round">
      <path d="M300 446 C340 430 370 452 392 440 S440 452 470 444" stroke="#d33"/>
      <path d="M310 449 C350 440 360 450 395 447 S450 438 500 448" stroke="#2d6cdf"/>
      <path d="M236 447 C270 432 290 452 330 445 S380 438 420 449" stroke="#222"/>
      <path d="M520 449 C545 440 560 450 585 446" stroke="#e6b800"/>
    </g>

    <!-- the sad guy on a crate -->
    <rect x="296" y="418" width="50" height="32" fill="#b58a4f" stroke="#6b4a22" stroke-width="2"/>
    <path d="M296 428 L346 428 M296 438 L346 438" stroke="#6b4a22" stroke-width="1.5"/>
    <path d="M298 420 Q300 396 322 394 Q344 396 346 420 Z" fill="#6b6f7a" stroke="#222" stroke-width="2"/>
    <g stroke="#222" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
      <ellipse cx="322" cy="376" rx="18" ry="22" fill="#f4f4f1"/>
      <path d="M310 370 L319 365 M334 370 L325 365" fill="none"/>
      <circle cx="315" cy="373" r="2.4" fill="#222"/><circle cx="329" cy="373" r="2.4" fill="#222"/>
      <path d="M314 392 Q322 385 330 392" fill="none"/>
      <path d="M312 377 Q310 383 312 387" fill="none" stroke="#4a8fd1"/>
      <path d="M304 378 Q302 385 305 391" fill="none" stroke="#999"/>
    </g>
    <!-- laptop on his lap, taped sticker -->
    <path d="M337 418 L366 418 L360 405 L342 405 Z" fill="#9aa3b2" stroke="#333" stroke-width="1.5"/>
    <rect x="344" y="395" width="20" height="12" fill="#1e3a6e" stroke="#333" stroke-width="1.5"/>
    <text x="354" y="404" text-anchor="middle" font-family="monospace" font-size="6" fill="#7cff7c">$_</text>

    <!-- Rack A: mismatched PCs stacked on a pallet, leaning -->
    <g stroke="#222" stroke-width="2" stroke-linejoin="round">
      <rect x="388" y="442" width="76" height="8" fill="#b88a55"/><path d="M388 446 L464 446" stroke="#6b4a22" stroke-width="1.5"/>
      <rect x="394" y="418" width="66" height="24" fill="#d8d0b8"/>
      <rect x="400" y="394" width="58" height="24" fill="#2b2b2b" transform="rotate(3 429 406)"/>
      <rect x="396" y="372" width="56" height="22" fill="#8d96a3" transform="rotate(-5 424 383)"/>
    </g>
    <g stroke="#222" stroke-width="1.5">
      <rect x="400" y="424" width="26" height="6" fill="#bdb49b"/><circle cx="446" cy="427" r="2.5" fill="#3f3"/><circle cx="453" cy="427" r="2.5" fill="#f33"/>
      <circle cx="446" cy="405" r="2.5" fill="#3f3" stroke="none" transform="rotate(3 429 406)"/><circle cx="452" cy="404" r="2.5" fill="#fa0" stroke="none" transform="rotate(3 429 406)"/>
      <circle cx="440" cy="381" r="2.5" fill="#f33" stroke="none" transform="rotate(-5 424 383)"/>
    </g>
    <!-- duct tape and a desk fan blowing on it all -->
    <path d="M398 392 L424 400" stroke="#b9b9b9" stroke-width="6" opacity=".85"/>
    <path d="M440 372 L444 388" stroke="#b9b9b9" stroke-width="5" opacity=".85"/>
    <g transform="translate(428 352)"><circle r="17" fill="#e8e8e8" stroke="#222" stroke-width="2"/><g fill="#6e8bb3" stroke="#222" stroke-width="1"><path d="M0 0 L-4 -13 Q0 -16 4 -13Z"/><path d="M0 0 L13 -4 Q16 0 13 4Z" /><path d="M0 0 L4 13 Q0 16 -4 13Z"/><path d="M0 0 L-13 4 Q-16 0 -13 -4Z"/></g><circle r="3" fill="#333"/></g>
    <path d="M428 369 L428 372" stroke="#222" stroke-width="3"/>
    <!-- dangling cables off rack A -->
    <g fill="none" stroke-width="2.5" stroke-linecap="round">
      <path d="M394 404 C380 410 388 428 376 440" stroke="#d33"/><path d="M394 392 C376 398 382 414 370 424" stroke="#2d6cdf"/>
      <path d="M460 396 C474 404 468 420 480 432" stroke="#e6b800"/>
    </g>

    <!-- Rack B: wire shelf with a router, a laptop and a power strip -->
    <g stroke="#666" stroke-width="2"><path d="M474 380 L474 450 M540 380 L540 450"/><path d="M470 396 L544 396 M470 424 L544 424"/></g>
    <g stroke="#222" stroke-width="1.5">
      <rect x="480" y="384" width="34" height="12" fill="#222"/><path d="M486 384 L482 368 M508 384 L514 368" stroke-width="2"/>
      <circle cx="490" cy="390" r="1.8" fill="#3f3" stroke="none"/><circle cx="498" cy="390" r="1.8" fill="#3f3" stroke="none"/><circle cx="506" cy="390" r="1.8" fill="#f33" stroke="none"/>
      <rect x="520" y="378" width="16" height="18" fill="#8c3b8f" transform="rotate(7 528 387)"/>
      <path d="M482 424 L532 424 L526 410 L488 410 Z" fill="#9aa3b2"/>
      <rect x="490" y="398" width="34" height="14" fill="#1e3a6e" transform="rotate(-4 507 405)"/>
    </g>
    <rect x="490" y="441" width="46" height="9" rx="3" fill="#eee" stroke="#222" stroke-width="1.5"/>
    <g fill="#222"><circle cx="498" cy="445" r="1.6"/><circle cx="506" cy="445" r="1.6"/><circle cx="514" cy="445" r="1.6"/><circle cx="522" cy="445" r="1.6"/><circle cx="530" cy="445" r="1.6"/></g>
    <g fill="none" stroke-width="2.5" stroke-linecap="round"><path d="M490 445 C470 440 468 452 452 446" stroke="#222"/><path d="M536 445 C552 438 560 450 572 444" stroke="#e6b800"/><path d="M500 424 C496 434 506 436 504 441" stroke="#2d6cdf"/></g>
    <!-- sticky note on rack A, taped sign leaning on the power strip -->
    <g transform="rotate(-8 415 384)"><rect x="403" y="375" width="28" height="20" fill="#fff35c" stroke="#b8a900" stroke-width="1"/><text x="417" y="389" text-anchor="middle" font-family="Comic Sans MS, cursive" font-size="9" font-weight="bold" fill="#c00">PROD</text></g>
    <g transform="rotate(5 546 437)"><rect x="522" y="428" width="48" height="15" fill="#fff" stroke="#999" stroke-width="1"/><text x="546" y="438.5" text-anchor="middle" font-family="Comic Sans MS, cursive" font-size="6.2" fill="#222">DO NOT UNPLUG!!</text><path d="M524 428 L530 428 M562 443 L568 443" stroke="#b9b9b9" stroke-width="4"/></g>
  </g>
  <!-- attic floor beam -->
  <rect x="190" y="448" width="420" height="12" fill="#7a4a2a" stroke="#4d2d18" stroke-width="3"/>

  <!-- ground floor -->
  <rect x="215" y="460" width="370" height="115" fill="#f0d9a8" stroke="#8a6b3a" stroke-width="3"/>
  <rect x="245" y="490" width="62" height="52" fill="#bfe3f5" stroke="#8a6b3a" stroke-width="3"/><line x1="276" y1="490" x2="276" y2="542" stroke="#8a6b3a" stroke-width="3"/>
  <rect x="493" y="490" width="62" height="52" fill="#bfe3f5" stroke="#8a6b3a" stroke-width="3"/><line x1="524" y1="490" x2="524" y2="542" stroke="#8a6b3a" stroke-width="3"/>
  <rect x="378" y="508" width="44" height="67" fill="#7a4a2a" stroke="#4d2d18" stroke-width="3"/><circle cx="412" cy="543" r="3" fill="#e5c25a"/>
  <!-- the extension cord that runs out of the attic window to the outlet below -->
  <path d="M586 452 C612 462 616 520 596 560" fill="none" stroke="#e6b800" stroke-width="3" stroke-linecap="round"/>

  <!-- trees -->
  <g><rect x="676" y="515" width="14" height="60" fill="#7a4a2a"/><circle cx="683" cy="500" r="38" fill="#4c8a3b"/><circle cx="660" cy="517" r="26" fill="#57973f"/><circle cx="708" cy="517" r="26" fill="#57973f"/>
  <rect x="120" y="540" width="10" height="35" fill="#7a4a2a"/><circle cx="125" cy="530" r="24" fill="#4c8a3b"/></g>

  <!-- label -->
  <path d="M130 372 Q210 372 296 380" stroke="#10203a" stroke-width="3" fill="none" stroke-linecap="round" marker-end="url(#arr)"/>
  <text x="40" y="366" font-family="Georgia, serif" font-size="22" fill="#10203a" font-weight="bold">mom's attic</text>
  <text x="400" y="620" text-anchor="middle" font-family="Georgia, serif" font-size="19" fill="#ffffff" font-style="italic">Cloud computing is too expensive, but at least we have mom's attic.</text>
</svg>
'''
(R/"attic-cloud.svg").write_text(svg)
print(len(svg))
