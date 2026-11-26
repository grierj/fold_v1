# Room calendar link

Run `verify-fold up` and `doctor`, then use the **admin** seed from `drive-ui.mjs`. Enable room booking and add a room through the admin forms or their API calls.

In two browser contexts signed in as admins, open **Admin → Rooms** at 390px. In the room's **More** menu, select **Calendar feed for** that room.

1. Open both panels before creating a link. Create it in the first panel. The link field has focus and the **Copy link** button is available.
2. Choose **Create calendar link** in the second panel. The response is 409. The panel shows the link is on and focuses the line explaining that the choice could not be saved. The first link still serves the calendar. The second panel displays no URL or **Copy link** button.
3. Choose **Make a new link** in the second panel and confirm. In the first panel, confirm **Turn off calendar link**. Check that the same refusal line takes focus, the old URL and **Copy link** disappear, and the replacement still serves the calendar.
4. Confirm **Turn off calendar link** again from the refreshed panel. It shows that the link is off and offers **Create calendar link**.

Capture the empty, newly created, refused-create, refused-turn-off and turned-off states at 390px. Mask the one-time URL in screenshots. Run the shared axe and layout checks on the refusal state.

For an unreadable answer, start with the link off. Let Create reach Fold and replace its HTTP 200 body with HTML in the browser route. Give the automatic status read an unreadable 200 too. The panel should explain that the change is uncertain and offer **Try again** with no link actions. Restore normal responses and choose **Try again**. The panel should show the link is on, with no URL or **Copy link**. Verify the browser sent only one POST and capture both states at 390px.
