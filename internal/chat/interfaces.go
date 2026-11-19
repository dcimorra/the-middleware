package chat

// InterfaceRule is said to every agent that may build something a person
// looks at: a site's conversation, the house's, the network agent, a crew
// agent, and a site's own conversation that can change files. It is in
// the preamble and not only in the guide for the reason TurnCommands are:
// what is only in the guide is read when the agent thinks the question is
// about building, and by then the page is drawn.
//
// Left to themselves the agents reached for emoji and Unicode symbols
// wherever an icon was wanted (a padlock, a clock, a tick), which every
// system draws in its own colours and its own shapes and which no
// stylesheet reaches, and delivered the first interface that worked:
// unstyled controls, no states, one width. The operator corrected both
// by hand in prompt after prompt ("no system icons") and the result was
// right every time, so the correction is the rule.
const InterfaceRule = "Whenever you build or change something a person looks at (a page, a component, a dashboard, a form, an email, a tool of your own with a screen), two rules hold without being asked, and a request that does not mention them does not lift them.\n" +
	"  - Icons are never system icons. No emoji and no Unicode symbol or dingbat standing in for an icon (a padlock, a clock, a tick, a cross, a star, a gear, a warning sign, an arrow used as a button), in markup, in CSS `content`, in a button's text or in a heading; and no icon left to a font the visitor's system happens to have. An icon is an inline SVG line icon from one consistent outline set: the set the project already uses, otherwise Lucide (Heroicons outline, Tabler and Phosphor are of the same kind). Drawn on a 24-unit grid with `fill=\"none\"`, `stroke=\"currentColor\"`, a stroke of 1.5 to 2 and round caps and joins, sized and coloured with the text beside it, the same set and the same stroke on every screen. A decorative one is `aria-hidden`; one that stands alone gets an `aria-label` on its button.\n" +
	"  - An interface is finished work, never a bare one. No unstyled browser defaults and no \"simple version for now\": a deliberate layout with a clear hierarchy, one spacing scale, a small considered palette and typography chosen on purpose; every control with its hover, focus, active and disabled states; the empty, loading and error states designed, not forgotten; laid out for a phone and for a wide screen; in light and in dark where the project has both; readable contrast and visible keyboard focus. If the project has a design system or existing screens, match them; if it has none, give it one and keep to it. Look at what you made as a visitor would (open it, at two widths) before you call it done.\n"
