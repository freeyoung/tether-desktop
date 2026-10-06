# Patched copy of fyne.io/systray v1.12.2

This is fyne.io/systray v1.12.2 (Apache License 2.0, see LICENSE), with 4
changes:

- `systray_windows.go`: every menu gets the `MNS_CHECKORBMP` style
  (`createMenu`, `setCheckOrBmp`, `convertToSubMenu`). With it, check marks
  and item bitmaps share 1 column, as in native Windows 11 menus. Without it,
  a menu that has both puts the text of items without a bitmap 2 columns in.
- `systray_windows.go`: a menu item icon is loaded at the small icon size
  (`loadMenuIconFrom`), which is the size the menu draws it at. The original
  loads the large default size and shrinks it, which blurs it.
- `systray_darwin.m`: `setIcon` makes the menu bar icon 16 pt high and keeps
  its aspect ratio. The original makes every icon 16 x 16 pt, which squeezes
  a wide icon.
- `systray_darwin.m`: a menu item icon is the item's `onStateImage`, and the
  item stays in the on state. So icons and check marks share 1 column, as
  with `MNS_CHECKORBMP` on Windows. As `image`, an icon gets a column of its
  own, and only the items that have one are indented.

The example and the tests of the original module are left out.
