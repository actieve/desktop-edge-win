﻿using System;
using System.Collections.Generic;
using System.Linq;
using System.Windows;
using System.Windows.Controls;
using System.Windows.Input;
using ZitiDesktopEdge.Models;
using ZitiDesktopEdge.ServiceClient;
using System.Windows.Media.Animation;

using NLog;
using System.Web;

namespace ZitiDesktopEdge {
	/// <summary>
	/// Interaction logic for IdentityDetails.xaml
	/// </summary> 
	public partial class IdentityDetails:UserControl {
		private static readonly Logger Logger = LogManager.GetCurrentClassLogger();

		private bool _isAttached = true;
		public delegate void Forgot(ZitiIdentity forgotten);
		public event Forgot OnForgot;
		public delegate void ErrorOccurred(string message);
		public event ErrorOccurred OnError;
		public delegate void MFAToggled(bool isOn);
		public event MFAToggled OnMFAToggled;
		public delegate void Detched(MouseButtonEventArgs e);
		public event Detched OnDetach;
		public double MainHeight = 500;
		public string filter = "";
		public delegate void Mesage(string message);
		public event Mesage OnMessage;
		public delegate void OnAuthenticate(ZitiIdentity identity);
		public event OnAuthenticate Authenticate;
		public delegate void OnRecovery(ZitiIdentity identity);
		public event OnRecovery Recovery;
		public int Page = 1;
		public int PerPage = 50;
		public int TotalPages = 1;
		public string SortBy = "Name";
		public string SortWay = "Asc";
		private bool _loaded = false;
		private double scrolledTo = 0;

		internal MainWindow MainWindow { get; set; }

		private List<ZitiIdentity> identities {
			get {
				return (List<ZitiIdentity>)Application.Current.Properties["Identities"];
			}
		}

		private ZitiIdentity _identity;

		public ZitiIdentity Identity {
			get {
				return _identity;
			}
			set {
				_loaded = false;
				scrolledTo = 0;
				_identity = value;
				this.IdDetailToggle.Enabled = _identity.IsEnabled;
				Page = 1;
				SortBy = "Name";
				SortWay = "Asc";
				SortByField.SelectedIndex = 0;
				SortWayField.SelectedIndex = 0;
				UpdateView();
				IdentityArea.Opacity = 1.0;
				IdentityArea.Visibility = Visibility.Visible;
				this.Visibility = Visibility.Visible;
			}
		}

		public IdentityItem SelectedIdentity { get; set; }
		public MenuIdentityItem SelectedIdentityMenu { get; set; }

		private void Window_MouseDown(object sender, MouseButtonEventArgs e) {
			if (e.ChangedButton == MouseButton.Left) {
				_isAttached = false;
				OnDetach(e);
			}
		}


		public bool IsAttached {
			get {
				return _isAttached;
			}
			set {
				_isAttached = value;
				if (_isAttached) {
					Arrow.Visibility = Visibility.Visible;
					ConfirmArrow.Visibility = Visibility.Visible;
				} else {
					Arrow.Visibility = Visibility.Collapsed;
					ConfirmArrow.Visibility = Visibility.Collapsed;
				}
			}
		}

		public void UpdateView() {
			ServiceScroller.InvalidateScrollInfo();
			ServiceScroller.ScrollToVerticalOffset(0);
			ServiceScroller.InvalidateScrollInfo();
			scrolledTo = 0;
			IdDetailName.Text = _identity.Name;
			IdDetailName.ToolTip = _identity.Name;
			IdDetailToggle.Enabled = _identity.IsEnabled;
			IdentityName.Value = _identity.Name;
			IdentityNetwork.Value = _identity.ControllerUrl;
			IdentityEnrollment.Value = _identity.EnrollmentStatus;
			IdentityStatus.Value = _identity.IsEnabled ? "active" : "disabled";
			IdentityMFA.IsOn = _identity.IsMFAEnabled;
			IdentityMFA.ToggleField.IsEnabled = true;
			IdentityMFA.ToggleField.Opacity = 1;
			if (_identity.IsMFAEnabled) {
				if (_identity.MFAInfo.IsAuthenticated) {
					IdentityMFA.ToggleField.IsEnabled = true;
					IdentityMFA.AuthOn.Visibility = Visibility.Visible;
					IdentityMFA.AuthOff.Visibility = Visibility.Collapsed;
					IdentityMFA.RecoveryButton.Visibility = Visibility.Visible;
				} else {
					IdentityMFA.ToggleField.Opacity = 0.2;
					IdentityMFA.ToggleField.IsEnabled = false;
					IdentityMFA.AuthOn.Visibility = Visibility.Collapsed;
					IdentityMFA.AuthOff.Visibility = Visibility.Visible;
					IdentityMFA.RecoveryButton.Visibility = Visibility.Collapsed;
				}
			} else {
				IdentityMFA.AuthOn.Visibility = Visibility.Collapsed;
				IdentityMFA.AuthOff.Visibility = Visibility.Collapsed;
				IdentityMFA.RecoveryButton.Visibility = Visibility.Collapsed;
			}
			ServiceList.Children.Clear();
			PageSortArea.Visibility = Visibility.Collapsed;
			if (_identity.Services.Count>0) {
				PageSortArea.Visibility = Visibility.Visible;
				int index = 0;
				int total = 0;
				ZitiService[] services = new ZitiService[0];
				if (SortBy == "Name") services = _identity.Services.OrderBy(s => s.Name.ToLower()).ToArray();
				else if (SortBy == "Address") services = _identity.Services.OrderBy(s => s.Addresses.ToString()).ToArray();
				else if (SortBy == "Protocol") services = _identity.Services.OrderBy(s => s.Protocols.ToString()).ToArray();
				else if (SortBy == "Port") services = _identity.Services.OrderBy(s => s.Ports.ToString()).ToArray();
				if (SortWay == "Desc") services = services.Reverse().ToArray();
				int startIndex = (Page - 1) * PerPage;
				for (int i= startIndex; i<services.Length; i++) {
					ZitiService zitiSvc = services[i];
					total++;
					if (index<PerPage) {
						if (zitiSvc.Name.ToLower().IndexOf(filter.ToLower()) >= 0 || zitiSvc.ToString().ToLower().IndexOf(filter.ToLower()) >= 0) {
							Logger.Trace("painting: " + zitiSvc.Name);
							ServiceInfo info = new ServiceInfo();
							info.Info = zitiSvc;
							info.OnMessage += Info_OnMessage;
							info.OnDetails += ShowDetails;
							ServiceList.Children.Add(info);
							index++;
						}
					}
				}

				TotalPages = (total / PerPage) + 1;

				double newHeight = MainHeight - 330; 
				ServiceRow.Height = new GridLength((double)newHeight);
				MainDetailScroll.MaxHeight = newHeight;
				MainDetailScroll.Height = newHeight;
				MainDetailScroll.Visibility = Visibility.Visible;
				ServiceTitle.Label = _identity.Services.Count + " Services";
				ServiceTitle.MainEdit.Visibility = Visibility.Visible;
				ServiceTitle.Visibility = Visibility.Visible;
				AuthMessageBg.Visibility = Visibility.Collapsed;
				AuthMessageLabel.Visibility = Visibility.Collapsed;
				NoAuthServices.Visibility = Visibility.Collapsed;
			} else {
				ServiceRow.Height = new GridLength((double)0.0);
				MainDetailScroll.Visibility = Visibility.Collapsed;
				ServiceTitle.Label = "No Services";
				ServiceTitle.MainEdit.Text = "";
				ServiceTitle.MainEdit.Visibility = Visibility.Collapsed;
				if (this._identity.IsMFAEnabled&&!this._identity.MFAInfo.IsAuthenticated) {
					ServiceTitle.Visibility = Visibility.Collapsed;
					AuthMessageBg.Visibility = Visibility.Visible;
					AuthMessageLabel.Visibility = Visibility.Visible;
					ServiceRow.Height = new GridLength((double)30);
					NoAuthServices.Visibility = Visibility.Visible;
				} else {
					ServiceTitle.Visibility = Visibility.Visible;
					AuthMessageBg.Visibility = Visibility.Collapsed;
					AuthMessageLabel.Visibility = Visibility.Collapsed;
					NoAuthServices.Visibility = Visibility.Collapsed;
				}
			}
			ConfirmView.Visibility = Visibility.Collapsed;
			_loaded = true;
		}

		private void ShowDetails(ZitiService info) {
			DetailName.Text = info.Name;
			DetailUrl.Text = info.ToString();

			string protocols = "";
			string addresses = "";
			string ports = "";

			for (int i = 0; i < info.Protocols.Length; i++) {
				protocols += ((i > 0) ? "," : "") + info.Protocols[i];
			}
			for (int i = 0; i < info.Addresses.Length; i++) {
				addresses += ((i>0)?",":"")+info.Addresses[i];
			}
			for (int i = 0; i < info.Ports.Length; i++) {
				ports += ((i > 0) ? "," : "") + info.Ports[i];
			}

			DetailProtocols.Text = protocols;
			DetailAddress.Text = addresses;
			DetailPorts.Text = ports;

			DetailsArea.Visibility = Visibility.Visible;
			DetailsArea.Opacity = 0;
			DetailsArea.Margin = new Thickness(0, 0, 0, 0);
			DoubleAnimation animation = new DoubleAnimation(1, TimeSpan.FromSeconds(.3));
			animation.Completed += ShowCompleted;
			DetailsArea.BeginAnimation(Grid.OpacityProperty, animation);
			DetailsArea.BeginAnimation(Grid.MarginProperty, new ThicknessAnimation(new Thickness(30, 30, 30, 30), TimeSpan.FromSeconds(.3)));

			ShowModal();
		}

		private void ShowCompleted(object sender, EventArgs e) {
			DoubleAnimation animation = new DoubleAnimation(DetailPanel.ActualHeight + 60, TimeSpan.FromSeconds(.3));
			DetailsArea.BeginAnimation(Grid.HeightProperty, animation);
			//DetailsArea.Height = DetailPanel.ActualHeight + 60;
		}

		private void CloseDetails(object sender, MouseButtonEventArgs e) {
			DoubleAnimation animation = new DoubleAnimation(0, TimeSpan.FromSeconds(.3));
			ThicknessAnimation animateThick = new ThicknessAnimation(new Thickness(0, 0, 0, 0), TimeSpan.FromSeconds(.3));
			DoubleAnimation animation2 = new DoubleAnimation(DetailPanel.ActualHeight + 100, TimeSpan.FromSeconds(.3));
			animation.Completed += HideComplete;
			DetailsArea.BeginAnimation(Grid.HeightProperty, animation2);
			DetailsArea.BeginAnimation(Grid.OpacityProperty, animation);
			DetailsArea.BeginAnimation(Grid.MarginProperty, animateThick);
			HideModal();
		}

		private void HideComplete(object sender, EventArgs e) {
			DetailsArea.Visibility = Visibility.Collapsed;
			ModalBg.Visibility = Visibility.Collapsed;
		}

		private void Info_OnMessage(string message) {
			OnMessage?.Invoke(message);
		}

		async private void IdToggle(bool on) {
			DataClient client = (DataClient)Application.Current.Properties["ServiceClient"];
			await client.IdentityOnOffAsync(_identity.Fingerprint, on);
			if (SelectedIdentity!=null) SelectedIdentity.ToggleSwitch.Enabled = on;
			if (SelectedIdentityMenu != null) SelectedIdentityMenu.ToggleSwitch.Enabled = on;
			_identity.IsEnabled = on;
			IdentityStatus.Value = _identity.IsEnabled ? "active" : "disabled";
		}

		public IdentityDetails() {
			InitializeComponent();
		}
		private void HideMenu(object sender, MouseButtonEventArgs e) {
			this.Visibility = Visibility.Collapsed;
		}

		public void SetHeight(double height) {
			MainDetailScroll.Height = height;
		}

		private void ForgetIdentity(object sender, MouseButtonEventArgs e) {
			if (this.Visibility==Visibility.Visible&&ConfirmView.Visibility==Visibility.Collapsed) {
				ConfirmView.Visibility = Visibility.Visible;
			}
		}

		private void CancelConfirmButton_Click(object sender, RoutedEventArgs e) {
			ConfirmView.Visibility = Visibility.Collapsed;
		}

		async private void ConfirmButton_Click(object sender, RoutedEventArgs e) {
			this.Visibility = Visibility.Collapsed;
			DataClient client = (DataClient)Application.Current.Properties["ServiceClient"];
			try {
				ConfirmView.Visibility = Visibility.Collapsed;
				await client.RemoveIdentityAsync(_identity.Fingerprint);

				ZitiIdentity forgotten = new ZitiIdentity();
				foreach (var id in identities) {
					if (id.Fingerprint == _identity.Fingerprint) {
						forgotten = id;
						identities.Remove(id);
						break;
					}
				}

				OnForgot?.Invoke(forgotten);
			} catch (DataStructures.ServiceException se) {
				Logger.Error(se, se.Message);
				OnError(se.Message);
			} catch (Exception ex) {
				Logger.Error(ex, "Unexpected: "+ ex.Message);
				OnError("An unexpected error has occured while removing the identity. Please verify the service is still running and try again.");
			}
		}

		private void DoFilter(string filter) {
			this.filter = filter;
			if (this._identity!=null) UpdateView();
		}


		private void ToggleMFA(bool isOn) {
			this.OnMFAToggled?.Invoke(isOn);
		}

		/* Modal UI Background visibility */

		/// <summary>
		/// Show the modal, aniimating opacity
		/// </summary>
		private void ShowModal() {
			ModalBg.Visibility = Visibility.Visible;
			ModalBg.Opacity = 0;
			ModalBg.BeginAnimation(Grid.OpacityProperty, new DoubleAnimation(.8, TimeSpan.FromSeconds(.3)));
		}

		/// <summary>
		/// Hide the modal animating the opacity
		/// </summary>
		private void HideModal() {
			DoubleAnimation animation = new DoubleAnimation(0, TimeSpan.FromSeconds(.3));
			animation.Completed += ModalHideComplete;
			ModalBg.BeginAnimation(Grid.OpacityProperty, animation);
		}

		/// <summary>
		/// When the animation completes, set the visibility to avoid UI object conflicts
		/// </summary>
		/// <param name="sender">The animation</param>
		/// <param name="e">The event</param>
		private void ModalHideComplete(object sender, EventArgs e) {
			ModalBg.Visibility = Visibility.Collapsed;
		}

		private void MFARecovery() {
			this.Recovery?.Invoke(this.Identity);
		}

		private void MFAAuthenticate() {
			this.Authenticate.Invoke(this.Identity);
		}

		private void AuthFromMessage(object sender, MouseButtonEventArgs e) {
			this.Authenticate.Invoke(this.Identity);
		}

		private void SortByField_SelectionChanged(object sender, SelectionChangedEventArgs e) {
			ComboBoxItem selected = (ComboBoxItem)SortByField.SelectedValue;
			if (selected != null && selected.Content!=null) {
				if (selected.Content.ToString()!=SortBy) {
					SortBy = selected.Content.ToString();
					UpdateView();
				}
			}
		}

		private void SortWayField_SelectionChanged(object sender, SelectionChangedEventArgs e) {
			ComboBoxItem selected = (ComboBoxItem)SortWayField.SelectedValue;
			if (selected != null && selected.Content != null) {
				if (selected.Content.ToString() != SortWay) {
					SortWay = selected.Content.ToString();
					UpdateView();
				}
			}
		}

		private void Scrolled(object sender, ScrollChangedEventArgs e) {
			if (_loaded) {
				var verticalOffset = ServiceScroller.VerticalOffset;
				var maxVerticalOffset = ServiceScroller.ScrollableHeight;

				if ((maxVerticalOffset < 0 || verticalOffset == maxVerticalOffset) && verticalOffset>0 && scrolledTo<verticalOffset) {
					if (Page < TotalPages) {
						scrolledTo = verticalOffset;
						ServiceScroller.ScrollChanged -= Scrolled;
						Logger.Trace("Paging: " + Page);
						_loaded = false;
						Page += 1;
						int index = 0;
						ZitiService[] services = new ZitiService[0];
						if (SortBy == "Name") services = _identity.Services.OrderBy(s => s.Name.ToLower()).ToArray();
						else if (SortBy == "Address") services = _identity.Services.OrderBy(s => s.Addresses.ToString()).ToArray();
						else if (SortBy == "Protocol") services = _identity.Services.OrderBy(s => s.Protocols.ToString()).ToArray();
						else if (SortBy == "Port") services = _identity.Services.OrderBy(s => s.Ports.ToString()).ToArray();
						if (SortWay == "Desc") services = services.Reverse().ToArray();
						int startIndex = (Page - 1) * PerPage;
						for (int i = startIndex; i < services.Length; i++) {
							ZitiService zitiSvc = services[i];
							if (index < PerPage) {
								if (zitiSvc.Name.ToLower().IndexOf(filter.ToLower()) >= 0 || zitiSvc.ToString().ToLower().IndexOf(filter.ToLower()) >= 0) {
									ServiceInfo info = new ServiceInfo();
									info.Info = zitiSvc;
									info.OnMessage += Info_OnMessage;
									info.OnDetails += ShowDetails;
									ServiceList.Children.Add(info);
									index++;
								}
							}
						}
						double totalOffset = ServiceScroller.VerticalOffset;
						double toNegate = index * 33;
						double scrollTo = (totalOffset - toNegate);
						ServiceScroller.InvalidateScrollInfo();
						ServiceScroller.ScrollToVerticalOffset(verticalOffset);
						ServiceScroller.InvalidateScrollInfo();
						_loaded = true;
						ServiceScroller.ScrollChanged += Scrolled;
					}
				}
			}
		}
	}
}
